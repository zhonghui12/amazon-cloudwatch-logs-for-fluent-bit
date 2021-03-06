// Copyright 2019 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package cloudwatch

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/amazon-kinesis-firehose-for-fluent-bit/plugins"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials/endpointcreds"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"
	"github.com/aws/aws-sdk-go/aws/endpoints"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	fluentbit "github.com/fluent/fluent-bit-go/output"
	jsoniter "github.com/json-iterator/go"
	"github.com/sirupsen/logrus"
)

const (
	// See: http://docs.aws.amazon.com/AmazonCloudWatchLogs/latest/APIReference/API_PutLogEvents.html
	perEventBytes          = 26
	maximumBytesPerPut     = 1048576
	maximumLogEventsPerPut = 10000
	maximumTimeSpanPerPut  = time.Hour * 24
)

const (
	// Log stream objects that are empty and inactive for longer than the timeout get cleaned up
	logStreamInactivityTimeout = time.Hour
	// Check for expired log streams every 10 minutes
	logStreamInactivityCheckInterval = 10 * time.Minute
)

// LogsClient contains the CloudWatch API calls used by this plugin
type LogsClient interface {
	CreateLogGroup(input *cloudwatchlogs.CreateLogGroupInput) (*cloudwatchlogs.CreateLogGroupOutput, error)
	PutRetentionPolicy(input *cloudwatchlogs.PutRetentionPolicyInput) (*cloudwatchlogs.PutRetentionPolicyOutput, error)
	CreateLogStream(input *cloudwatchlogs.CreateLogStreamInput) (*cloudwatchlogs.CreateLogStreamOutput, error)
	DescribeLogStreams(input *cloudwatchlogs.DescribeLogStreamsInput) (*cloudwatchlogs.DescribeLogStreamsOutput, error)
	PutLogEvents(input *cloudwatchlogs.PutLogEventsInput) (*cloudwatchlogs.PutLogEventsOutput, error)
}

type logStream struct {
	logEvents         []*cloudwatchlogs.InputLogEvent
	currentByteLength int
	currentBatchStart *time.Time
	currentBatchEnd   *time.Time
	nextSequenceToken *string
	logStreamName     string
	logGroupName      string
	expiration        time.Time
}

// Event is the input data and contains a log entry.
// The group and stream are added during processing.
type Event struct {
	TS     time.Time
	Record map[interface{}]interface{}
	Tag    string
	group  string
	stream string
}

func (stream *logStream) isExpired() bool {
	if len(stream.logEvents) == 0 && stream.expiration.Before(time.Now()) {
		return true
	}
	return false
}

func (stream *logStream) updateExpiration() {
	stream.expiration = time.Now().Add(logStreamInactivityTimeout)
}

// OutputPlugin is the CloudWatch Logs Fluent Bit output plugin
type OutputPlugin struct {
	logGroupName                  string
	logStreamPrefix               string
	logStreamName                 string
	logKey                        string
	client                        LogsClient
	streams                       map[string]*logStream
	groups                        map[string]struct{}
	timer                         *plugins.Timeout
	nextLogStreamCleanUpCheckTime time.Time
	PluginInstanceID              int
	logGroupTags                  map[string]*string
	logGroupRetention             int64
}

// OutputPluginConfig is the input information used by NewOutputPlugin to create a new OutputPlugin
type OutputPluginConfig struct {
	Region           string
	LogGroupName     string
	LogStreamPrefix  string
	LogStreamName    string
	LogKey           string
	RoleARN          string
	NewLogGroupTags  string
	LogRetentionDays int64
	CWEndpoint       string
	STSEndpoint      string
	CredsEndpoint    string
	PluginInstanceID int
	LogFormat        string
}

// Validate checks the configuration input for an OutputPlugin instances
func (config OutputPluginConfig) Validate() error {
	errorStr := "%s is a required parameter"
	if config.Region == "" {
		return fmt.Errorf(errorStr, "region")
	}
	if config.LogGroupName == "" {
		return fmt.Errorf(errorStr, "log_group_name")
	}
	if config.LogStreamName == "" && config.LogStreamPrefix == "" {
		return fmt.Errorf("log_stream_name or log_stream_prefix is required")
	}

	return nil
}

// NewOutputPlugin creates a OutputPlugin object
func NewOutputPlugin(config OutputPluginConfig) (*OutputPlugin, error) {
	logrus.Debugf("[cloudwatch %d] Initializing NewOutputPlugin", config.PluginInstanceID)

	client, err := newCloudWatchLogsClient(config)
	if err != nil {
		return nil, err
	}

	timer, err := plugins.NewTimeout(func(d time.Duration) {
		logrus.Errorf("[cloudwatch %d] timeout threshold reached: Failed to send logs for %s\n", config.PluginInstanceID, d.String())
		logrus.Fatalf("[cloudwatch %d] Quitting Fluent Bit", config.PluginInstanceID) // exit the plugin and kill Fluent Bit
	})

	if err != nil {
		return nil, err
	}

	return &OutputPlugin{
		logGroupName:                  config.LogGroupName,
		logStreamPrefix:               config.LogStreamPrefix,
		logStreamName:                 config.LogStreamName,
		logKey:                        config.LogKey,
		client:                        client,
		timer:                         timer,
		streams:                       make(map[string]*logStream),
		nextLogStreamCleanUpCheckTime: time.Now().Add(logStreamInactivityCheckInterval),
		PluginInstanceID:              config.PluginInstanceID,
		logGroupTags:                  tagKeysToMap(config.NewLogGroupTags),
		logGroupRetention:             config.LogRetentionDays,
		groups:                        make(map[string]struct{}),
	}, nil
}

func newCloudWatchLogsClient(config OutputPluginConfig) (*cloudwatchlogs.CloudWatchLogs, error) {
	customResolverFn := func(service, region string, optFns ...func(*endpoints.Options)) (endpoints.ResolvedEndpoint, error) {
		if service == endpoints.LogsServiceID && config.CWEndpoint != "" {
			return endpoints.ResolvedEndpoint{
				URL: config.CWEndpoint,
			}, nil
		} else if service == endpoints.StsServiceID && config.STSEndpoint != "" {
			return endpoints.ResolvedEndpoint{
				URL: config.STSEndpoint,
			}, nil
		}
		return endpoints.DefaultResolver().EndpointFor(service, region, optFns...)
	}

	svcConfig := &aws.Config{
		Region:                        aws.String(config.Region),
		EndpointResolver:              endpoints.ResolverFunc(customResolverFn),
		CredentialsChainVerboseErrors: aws.Bool(true),
	}

	if config.CredsEndpoint != "" {
		creds := endpointcreds.NewCredentialsClient(*svcConfig, request.Handlers{}, config.CredsEndpoint,
			func(provider *endpointcreds.Provider) {
				provider.ExpiryWindow = 5 * time.Minute
			})
		svcConfig.Credentials = creds
	}

	sess, err := session.NewSession(svcConfig)
	if err != nil {
		return nil, err
	}

	stsConfig := &aws.Config{}
	if config.RoleARN != "" {
		creds := stscreds.NewCredentials(sess, config.RoleARN)
		stsConfig.Credentials = creds
	}

	client := cloudwatchlogs.New(sess, stsConfig)
	client.Handlers.Build.PushBackNamed(plugins.CustomUserAgentHandler())
	if config.LogFormat != "" {
		client.Handlers.Build.PushBackNamed(LogFormatHandler(config.LogFormat))
	}
	return client, nil
}

// AddEvent accepts a record and adds it to the buffer for its stream, flushing the buffer if it is full
// the return value is one of: FLB_OK, FLB_RETRY
// API Errors lead to an FLB_RETRY, and all other errors are logged, the record is discarded and FLB_OK is returned
func (output *OutputPlugin) AddEvent(e *Event) int {
	// Step 1: convert the Event data to strings, and check for a log key.
	data, err := output.processRecord(e)
	if err != nil {
		logrus.Errorf("[cloudwatch %d] %v\n", output.PluginInstanceID, err)
		// discard this single bad record and let the batch continue
		return fluentbit.FLB_OK
	}

	// Step 2. Make sure the Event data isn't empty.
	eventString := logString(data)
	if len(eventString) == 0 {
		logrus.Debugf("[cloudwatch %d] Discarding an event from publishing as it is empty\n", output.PluginInstanceID)
		// discard this single empty record and let the batch continue
		return fluentbit.FLB_OK
	}

	// Step 3. Assign a log group and log stream name to the Event.
	output.setGroupStreamNames(e)

	// Step 4. Create a missing log group for this Event.
	if _, ok := output.groups[e.group]; !ok {
		logrus.Debugf("[cloudwatch %d] Finding log group: %s", output.PluginInstanceID, e.group)

		if err := output.createLogGroup(e); err != nil {
			logrus.Error(err)
			return fluentbit.FLB_ERROR
		}

		output.groups[e.group] = struct{}{}
	}

	// Step 5. Create or retrieve an existing log stream for this Event.
	stream, err := output.getLogStream(e)
	if err != nil {
		logrus.Errorf("[cloudwatch %d] %v\n", output.PluginInstanceID, err)
		// an error means that the log stream was not created; this is retryable
		return fluentbit.FLB_RETRY
	}

	// Step 6. Check batch limits and flush buffer if any of these limits will be exeeded by this log Entry.
	countLimit := len(stream.logEvents) == maximumLogEventsPerPut
	sizeLimit := (stream.currentByteLength + cloudwatchLen(eventString)) >= maximumBytesPerPut
	spanLimit := stream.logBatchSpan(e.TS) >= maximumTimeSpanPerPut
	if countLimit || sizeLimit || spanLimit {
		err = output.putLogEvents(stream)
		if err != nil {
			logrus.Errorf("[cloudwatch %d] %v\n", output.PluginInstanceID, err)
			// send failures are retryable
			return fluentbit.FLB_RETRY
		}
	}

	// Step 7. Add this event to the running tally.
	stream.logEvents = append(stream.logEvents, &cloudwatchlogs.InputLogEvent{
		Message:   aws.String(eventString),
		Timestamp: aws.Int64(e.TS.UnixNano() / 1e6), // CloudWatch uses milliseconds since epoch
	})
	stream.currentByteLength += cloudwatchLen(eventString)
	if stream.currentBatchStart == nil || stream.currentBatchStart.After(e.TS) {
		stream.currentBatchStart = &e.TS
	}
	if stream.currentBatchEnd == nil || stream.currentBatchEnd.Before(e.TS) {
		stream.currentBatchEnd = &e.TS
	}

	return fluentbit.FLB_OK
}

// This plugin tracks CW Log streams
// We need to periodically delete any streams that haven't been written to in a while
// Because each stream incurs some memory for its buffer of log events
// (Which would be empty for an unused stream)
func (output *OutputPlugin) cleanUpExpiredLogStreams() {
	if output.nextLogStreamCleanUpCheckTime.Before(time.Now()) {
		logrus.Debugf("[cloudwatch %d] Checking for expired log streams", output.PluginInstanceID)

		for name, stream := range output.streams {
			if stream.isExpired() {
				logrus.Debugf("[cloudwatch %d] Removing internal buffer for log stream %s in group %s; the stream has not been written to for %s",
					output.PluginInstanceID, stream.logStreamName, stream.logGroupName, logStreamInactivityTimeout.String())
				delete(output.streams, name)
			}
		}
		output.nextLogStreamCleanUpCheckTime = time.Now().Add(logStreamInactivityCheckInterval)
	}
}

func (output *OutputPlugin) getLogStream(e *Event) (*logStream, error) {
	stream, ok := output.streams[e.group+e.stream]
	if !ok {
		// stream doesn't exist, create it
		stream, err := output.createStream(e)
		if err != nil {
			if awsErr, ok := err.(awserr.Error); ok {
				if awsErr.Code() == cloudwatchlogs.ErrCodeResourceAlreadyExistsException {
					// existing stream
					return output.existingLogStream(e)
				}
			}
		}
		return stream, err
	}

	return stream, nil
}

func (output *OutputPlugin) existingLogStream(e *Event) (*logStream, error) {
	var nextToken *string
	var stream *logStream

	for stream == nil {
		resp, err := output.describeLogStreams(e, nextToken)
		if err != nil {
			return nil, err
		}

		for _, result := range resp.LogStreams {
			if aws.StringValue(result.LogStreamName) == e.stream {
				stream = &logStream{
					logGroupName:      e.group,
					logStreamName:     e.stream,
					logEvents:         make([]*cloudwatchlogs.InputLogEvent, 0, maximumLogEventsPerPut),
					nextSequenceToken: result.UploadSequenceToken,
				}
				output.streams[e.group+e.stream] = stream

				logrus.Debugf("[cloudwatch %d] Initializing internal buffer for exising log stream %s\n", output.PluginInstanceID, e.stream)
				stream.updateExpiration() // initialize

				break
			}
		}

		if stream == nil && resp.NextToken == nil {
			return nil, fmt.Errorf("error: does not compute: Log Stream %s could not be created, but also could not be found in the log group", e.stream)
		}

		nextToken = resp.NextToken
	}
	return stream, nil
}

func (output *OutputPlugin) describeLogStreams(e *Event, nextToken *string) (*cloudwatchlogs.DescribeLogStreamsOutput, error) {
	output.timer.Check()
	resp, err := output.client.DescribeLogStreams(&cloudwatchlogs.DescribeLogStreamsInput{
		LogGroupName:        aws.String(e.group),
		LogStreamNamePrefix: aws.String(e.stream),
		NextToken:           nextToken,
	})

	if err != nil {
		output.timer.Start()
		return nil, err
	}
	output.timer.Reset()

	return resp, err
}

// setGroupStreamNames adds the log group and log stream names to the event struct.
// This happens by parsing (any) template data in either configured name.
func (output *OutputPlugin) setGroupStreamNames(e *Event) {
	// This happens here to avoid running Split more than once per log Event.
	logTagSplit := strings.SplitN(e.Tag, ".", 10)

	var err error
	if e.group, err = parseDataMapTags(e, logTagSplit, output.logGroupName); err != nil {
		logrus.Errorf("[cloudwatch %d] parsing template: '%s': %v", output.PluginInstanceID, output.logGroupName, err)
	}

	if e.group == "" {
		e.group = output.logGroupName
	}

	if output.logStreamPrefix != "" {
		e.stream = output.logStreamPrefix + e.Tag
		return
	}

	if e.stream, err = parseDataMapTags(e, logTagSplit, output.logStreamName); err != nil {
		// If a user gets this error, they need to fix their log_stream_name template to make it go away. Simple.
		logrus.Errorf("[cloudwatch %d] parsing template: '%s': %v", output.PluginInstanceID, output.logStreamName, err)
	}

	if e.stream == "" {
		e.stream = output.logStreamName
	}
}

func (output *OutputPlugin) createStream(e *Event) (*logStream, error) {
	output.timer.Check()
	_, err := output.client.CreateLogStream(&cloudwatchlogs.CreateLogStreamInput{
		LogGroupName:  aws.String(e.group),
		LogStreamName: aws.String(e.stream),
	})

	if err != nil {
		output.timer.Start()
		return nil, err
	}
	output.timer.Reset()

	stream := &logStream{
		logStreamName:     e.stream,
		logGroupName:      e.group,
		logEvents:         make([]*cloudwatchlogs.InputLogEvent, 0, maximumLogEventsPerPut),
		nextSequenceToken: nil, // sequence token not required for a new log stream
	}
	output.streams[e.group+e.stream] = stream
	stream.updateExpiration() // initialize
	logrus.Infof("[cloudwatch %d] Created log stream %s in group %s", output.PluginInstanceID, e.stream, e.group)

	return stream, nil
}

func (output *OutputPlugin) createLogGroup(e *Event) error {
	_, err := output.client.CreateLogGroup(&cloudwatchlogs.CreateLogGroupInput{
		LogGroupName: aws.String(e.group),
		Tags:         output.logGroupTags,
	})
	if err == nil {
		logrus.Infof("[cloudwatch %d] Created log group %s\n", output.PluginInstanceID, e.group)
		return output.setLogGroupRetention(e.group)
	}

	if awsErr, ok := err.(awserr.Error); !ok ||
		awsErr.Code() != cloudwatchlogs.ErrCodeResourceAlreadyExistsException {
		return err
	}

	logrus.Infof("[cloudwatch %d] Log group %s already exists\n", output.PluginInstanceID, e.group)
	return nil
}

func (output *OutputPlugin) setLogGroupRetention(name string) error {
	if output.logGroupRetention < 1 {
		return nil
	}

	_, err := output.client.PutRetentionPolicy(&cloudwatchlogs.PutRetentionPolicyInput{
		LogGroupName:    aws.String(name),
		RetentionInDays: aws.Int64(output.logGroupRetention),
	})
	if err != nil {
		return err
	}

	logrus.Infof("[cloudwatch %d] Set retention policy on log group %s to %dd\n", output.PluginInstanceID, name, output.logGroupRetention)

	return nil
}

// Takes the byte slice and returns a string
// Also removes leading and trailing whitespace
func logString(record []byte) string {
	return strings.TrimSpace(string(record))
}

func (output *OutputPlugin) processRecord(e *Event) ([]byte, error) {
	var err error
	e.Record, err = plugins.DecodeMap(e.Record)
	if err != nil {
		logrus.Debugf("[cloudwatch %d] Failed to decode record: %v\n", output.PluginInstanceID, e.Record)
		return nil, err
	}

	var json = jsoniter.ConfigCompatibleWithStandardLibrary
	var data []byte

	if output.logKey != "" {
		log, err := plugins.LogKey(e.Record, output.logKey)
		if err != nil {
			return nil, err
		}

		data, err = plugins.EncodeLogKey(log)
	} else {
		data, err = json.Marshal(e.Record)
	}

	if err != nil {
		logrus.Debugf("[cloudwatch %d] Failed to marshal record: %v\nLog Key: %s\n", output.PluginInstanceID, e.Record, output.logKey)
		return nil, err
	}

	return data, nil
}

// Flush sends the current buffer of records.
func (output *OutputPlugin) Flush() error {
	logrus.Debugf("[cloudwatch %d] Flush() Called", output.PluginInstanceID)

	for _, stream := range output.streams {
		if err := output.flushStream(stream); err != nil {
			return err
		}
	}

	return nil
}

func (output *OutputPlugin) flushStream(stream *logStream) error {
	output.cleanUpExpiredLogStreams() // will periodically clean up, otherwise is no-op
	return output.putLogEvents(stream)
}

func (output *OutputPlugin) putLogEvents(stream *logStream) error {
	// return in case of empty logEvents
	if len(stream.logEvents) == 0 {
		return nil
	}

	output.timer.Check()
	stream.updateExpiration()

	// Log events in a single PutLogEvents request must be in chronological order.
	sort.SliceStable(stream.logEvents, func(i, j int) bool {
		return aws.Int64Value(stream.logEvents[i].Timestamp) < aws.Int64Value(stream.logEvents[j].Timestamp)
	})
	response, err := output.client.PutLogEvents(&cloudwatchlogs.PutLogEventsInput{
		LogEvents:     stream.logEvents,
		LogGroupName:  aws.String(stream.logGroupName),
		LogStreamName: aws.String(stream.logStreamName),
		SequenceToken: stream.nextSequenceToken,
	})
	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok {
			if awsErr.Code() == cloudwatchlogs.ErrCodeDataAlreadyAcceptedException {
				// already submitted, just grab the correct sequence token
				parts := strings.Split(awsErr.Message(), " ")
				stream.nextSequenceToken = &parts[len(parts)-1]
				stream.logEvents = stream.logEvents[:0]
				stream.currentByteLength = 0
				stream.currentBatchStart = nil
				stream.currentBatchEnd = nil
				logrus.Infof("[cloudwatch %d] Encountered error %v; data already accepted, ignoring error\n", output.PluginInstanceID, awsErr)
				return nil
			} else if awsErr.Code() == cloudwatchlogs.ErrCodeInvalidSequenceTokenException {
				// sequence code is bad, grab the correct one and retry
				parts := strings.Split(awsErr.Message(), " ")
				stream.nextSequenceToken = &parts[len(parts)-1]

				return output.putLogEvents(stream)
			} else {
				output.timer.Start()
				return err
			}
		} else {
			return err
		}
	}
	output.processRejectedEventsInfo(response)
	output.timer.Reset()
	logrus.Debugf("[cloudwatch %d] Sent %d events to CloudWatch for stream '%s' in group '%s'",
		output.PluginInstanceID, len(stream.logEvents), stream.logStreamName, stream.logGroupName)

	stream.nextSequenceToken = response.NextSequenceToken
	stream.logEvents = stream.logEvents[:0]
	stream.currentByteLength = 0
	stream.currentBatchStart = nil
	stream.currentBatchEnd = nil

	return nil
}

func (output *OutputPlugin) processRejectedEventsInfo(response *cloudwatchlogs.PutLogEventsOutput) {
	if response.RejectedLogEventsInfo != nil {
		if response.RejectedLogEventsInfo.ExpiredLogEventEndIndex != nil {
			logrus.Warnf("[cloudwatch %d] %d log events were marked as expired by CloudWatch\n", output.PluginInstanceID, aws.Int64Value(response.RejectedLogEventsInfo.ExpiredLogEventEndIndex))
		}
		if response.RejectedLogEventsInfo.TooNewLogEventStartIndex != nil {
			logrus.Warnf("[cloudwatch %d] %d log events were marked as too new by CloudWatch\n", output.PluginInstanceID, aws.Int64Value(response.RejectedLogEventsInfo.TooNewLogEventStartIndex))
		}
		if response.RejectedLogEventsInfo.TooOldLogEventEndIndex != nil {
			logrus.Warnf("[cloudwatch %d] %d log events were marked as too old by CloudWatch\n", output.PluginInstanceID, aws.Int64Value(response.RejectedLogEventsInfo.TooOldLogEventEndIndex))
		}
	}
}

// effectiveLen counts the effective number of bytes in the string, after
// UTF-8 normalization.  UTF-8 normalization includes replacing bytes that do
// not constitute valid UTF-8 encoded Unicode codepoints with the Unicode
// replacement codepoint U+FFFD (a 3-byte UTF-8 sequence, represented in Go as
// utf8.RuneError)
func effectiveLen(line string) int {
	effectiveBytes := 0
	for _, rune := range line {
		effectiveBytes += utf8.RuneLen(rune)
	}
	return effectiveBytes
}

func cloudwatchLen(event string) int {
	return effectiveLen(event) + perEventBytes
}

func (stream *logStream) logBatchSpan(timestamp time.Time) time.Duration {
	if stream.currentBatchStart == nil || stream.currentBatchEnd == nil {
		return 0
	}

	if stream.currentBatchStart.After(timestamp) {
		return stream.currentBatchEnd.Sub(timestamp)
	} else if stream.currentBatchEnd.Before(timestamp) {
		return timestamp.Sub(*stream.currentBatchStart)
	}

	return stream.currentBatchEnd.Sub(*stream.currentBatchStart)
}
