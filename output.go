/**
 * K6 Extension that writes results to AWS Timestream.
 * Based on some of the outputs from the official K6
 * repo. See
 * https://github.com/grafana/k6/tree/master/output
 */

package timestream

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/timestreamwrite/types"

	"github.com/aws/aws-sdk-go-v2/service/timestreamwrite"

	"github.com/aws/aws-sdk-go-v2/config"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/sirupsen/logrus"

	"go.k6.io/k6/metrics"
	"go.k6.io/k6/output"
)

func init() {
	output.RegisterExtension("timestream", New)
}

type WriteClient interface {
	WriteRecords(
		ctx context.Context,
		params *timestreamwrite.WriteRecordsInput,
		optFns ...func(*timestreamwrite.Options),
	) (*timestreamwrite.WriteRecordsOutput, error)
}

type Output struct {
	client WriteClient
	config *Config
	logger logrus.FieldLogger

	metricSampleContainerQueue chan *metrics.SampleContainer
	doneWriting                chan bool
}

func New(params output.Params) (output.Output, error) {
	extensionConfig, err := GetConsolidatedConfig(params.JSONConfig)
	if err != nil {
		return nil, err
	}

	ctx := context.Background()

	awsConfig, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to load AWS config: %w", err)
	}

	if extensionConfig.Region != "" {
		awsConfig.Region = extensionConfig.Region
	}

	client := timestreamwrite.NewFromConfig(awsConfig)

	return &Output{
		client: client,
		config: &extensionConfig,
		logger: params.Logger.WithField("component", "timestream"),
	}, nil
}

func (o *Output) Description() string {
	return fmt.Sprintf(
		"Timestream (%s:%s)",
		o.config.DatabaseName,
		o.config.TableName,
	)
}

func (o *Output) Start() error {
	o.logger.Debug("starting...")
	o.metricSampleContainerQueue = make(chan *metrics.SampleContainer)
	o.doneWriting = make(chan bool)

	go o.metricSamplesHandler()
	o.logger.Debug("started!")

	return nil
}

func (o *Output) Stop() error {
	o.logger.Debug("stopping...")
	close(o.metricSampleContainerQueue)
	o.logger.Debug("closed MetricSampleContainerQueue")
	<-o.doneWriting
	o.logger.Debug("stopped!")

	return nil
}

func (o *Output) AddMetricSamples(samples []metrics.SampleContainer) {
	for _, sampleContainer := range samples {
		sampleContainer := sampleContainer
		o.metricSampleContainerQueue <- &sampleContainer
	}
}

/**
 * Pulls together all the metrics in one place in the correct format for timestream
 * so that it can write the metrics when it reaches the TimestreamMaxBatchSize.
 */
func (o *Output) metricSamplesHandler() {
	// See https://docs.aws.amazon.com/timestream/latest/developerguide/API_WriteRecords.html
	TimestreamMaxBatchSize := 100

	var (
		timestreamRecordsToSave []types.Record
		wg                      sync.WaitGroup
	)

	start := time.Now()

	for metricSampleContainer := range o.metricSampleContainerQueue {
		timestreamRecordsForContainer := o.createRecords((*metricSampleContainer).GetSamples())
		timestreamRecordsToSave = append(timestreamRecordsToSave, timestreamRecordsForContainer...)

		if len(timestreamRecordsToSave) > TimestreamMaxBatchSize {
			o.writeRecordsAsync(timestreamRecordsToSave[:TimestreamMaxBatchSize], &wg, &start)
			timestreamRecordsToSave = timestreamRecordsToSave[TimestreamMaxBatchSize:]
		}
	}

	if len(timestreamRecordsToSave) > 0 {
		o.writeRecordsAsync(timestreamRecordsToSave, &wg, &start)
	}

	wg.Wait()
	o.logger.Debug("metric samples handler done")
	o.doneWriting <- true
}

/**
 * Mapping from K6 metrics to AWS Timstream records.
 */
func (o *Output) createRecords(samples []metrics.Sample) []types.Record {
	records := make([]types.Record, 0, len(samples))

	for _, sample := range samples {
		var dimensions []types.Dimension

		for tagKey, tagValue := range sample.Tags.Map() {
			if len(strings.TrimSpace(tagValue)) == 0 {
				continue
			}

			dimensions = append(dimensions, types.Dimension{
				Name:  aws.String(tagKey),
				Value: aws.String(tagValue),
			})
		}

		for tagKey, tagValue := range sample.Metadata {
			if len(strings.TrimSpace(tagValue)) == 0 {
				continue
			}

			dimensions = append(dimensions, types.Dimension{
				Name:  aws.String(tagKey),
				Value: aws.String(tagValue),
			})
		}

		records = append(records, types.Record{
			Dimensions:       dimensions,
			MeasureName:      aws.String(sample.Metric.Name),
			MeasureValue:     aws.String(fmt.Sprintf("%.6f", sample.Value)),
			MeasureValueType: "DOUBLE",
			Time: aws.String(
				strconv.FormatInt(sample.GetTime().UnixNano(), 10),
			),
			TimeUnit: "NANOSECONDS",
		})
	}

	return records
}

/**
 * We perform the save to the database in a separate
 * thread as the network call is orders of magnitude
 * slower than running on the CPU and can be done in
 * parallel. This ultimately means we don't end up
 * waiting for a long time after the tests have
 * finished for data to be written to the database.
 */
func (o *Output) writeRecordsAsync(
	records []types.Record,
	waitGroup *sync.WaitGroup,
	startTime *time.Time,
) {
	waitGroup.Add(1)

	go func(recordsToSave *[]types.Record) {
		defer waitGroup.Done()

		logger := o.logger.
			WithField("count", len(*recordsToSave)).
			WithField("records_address", &recordsToSave)

		logger.WithField("t", time.Since(*startTime)).
			Debug("starting write")

		startWriteTime := time.Now()
		countSaved, err := o.writeRecords(recordsToSave)

		logger = logger.
			WithField("t", time.Since(*startTime)).
			WithField("duration", time.Since(startWriteTime))
		if err != nil {
			logTimestreamError(logger, err)

			return
		}

		logger.
			WithField("count_saved", countSaved).
			Debug("wrote metrics")
	}(&records)
}

func (o *Output) writeRecords(records *[]types.Record) (int32, error) {
	writeRecordsInput := &timestreamwrite.WriteRecordsInput{
		DatabaseName: aws.String(o.config.DatabaseName),
		TableName:    aws.String(o.config.TableName),
		Records:      *records,
	}

	ctx := context.Background()

	response, err := o.client.WriteRecords(ctx, writeRecordsInput)
	if err != nil {
		return 0, fmt.Errorf("unable to write records to timestream: %w", err)
	}

	return response.RecordsIngested.Total, nil
}

func logTimestreamError(logger logrus.FieldLogger, err error) {
	logger.
		WithError(err).
		Error("failed to write")

	var rejected *types.RejectedRecordsException
	if errors.As(err, &rejected) {
		for _, rr := range rejected.RejectedRecords {
			logger.Errorf(
				"reject reason: %q, record index: %d",
				aws.ToString(rr.Reason),
				rr.RecordIndex,
			)
		}
	}
}
