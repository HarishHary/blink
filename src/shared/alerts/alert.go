package alerts

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/google/uuid"
	"github.com/harishhary/blink/src/shared"
	"github.com/harishhary/blink/src/shared/helpers"
	"github.com/harishhary/blink/src/shared/publishers"
)

// AlertCreationError custom error for alert creation
type AlertError struct {
	Message string
}

func (e *AlertError) Error() string {
	return fmt.Sprintf("Alert Creation failed with error: %s", e.Message)
}

// Alert struct encapsulates a single alert and handles serialization
type Alert struct {
	AlertID         string
	Attempts        int
	Cluster         string
	Context         map[string]interface{}
	Created         time.Time
	Dispatched      time.Time
	LogSource       string
	LogType         string
	MergeByKeys     []string
	MergeWindow     time.Duration
	Outputs         []string
	OutputsSent     []string
	Publishers      []publishers.IPublisher
	Record          shared.Record
	RuleDescription string
	RuleName        string
	SourceEntity    string
	SourceService   string
	Staged          bool
}

// Constants for datetime format
const DATETIME_FORMAT = "2006-01-02T15:04:05.000Z"

// NewAlert creates a new Alert
func NewAlert(ruleName string, record shared.Record, outputs []string, opts ...AlertOption) (*Alert, error) {
	alert := &Alert{
		AlertID:     uuid.NewString(),
		Created:     time.Now().UTC(),
		RuleName:    ruleName,
		Record:      record,
		Outputs:     outputs,
		Context:     make(map[string]interface{}),
		Publishers:  []publishers.IPublisher{},
		OutputsSent: make([]string, 10),
	}

	for _, opt := range opts {
		opt(alert)
	}

	if !(alert.RuleName != "" && len(alert.Outputs) > 0) {
		return nil, &AlertError{Message: "Invalid Alert options"}
	}

	return alert, nil
}

// Merge merges multiple alerts into a new merged alert
func Merge(alerts []*Alert) (*Alert, error) {
	if len(alerts) == 0 {
		return nil, errors.New("no alerts to merge")
	}

	sort.Slice(alerts, func(i, j int) bool {
		return alerts[i].Created.Before(alerts[j].Created)
	})

	mergeKeys := alerts[0].MergeByKeys
	cleanedRecords := make([]shared.Record, len(alerts))
	for i, alert := range alerts {
		cleanedRecords[i] = alert.Record.CleanRecord(mergeKeys)
	}

	common := computeCommon(cleanedRecords)

	newRecord := shared.Record{
		"AlertCount":      len(alerts),
		"AlertTimeFirst":  alerts[0].Created.Format(DATETIME_FORMAT),
		"AlertTimeLast":   alerts[len(alerts)-1].Created.Format(DATETIME_FORMAT),
		"MergedBy":        alerts[0].Record.GetMergedKeys(mergeKeys),
		"OtherCommonKeys": common,
		"ValueDiffs":      getValueDiffs(common, alerts, cleanedRecords),
	}

	return NewAlert(
		alerts[0].RuleName,
		newRecord,
		alerts[len(alerts)-1].Outputs,
		Cluster(alerts[0].Cluster),
		Context(alerts[0].Context),
		LogSource(alerts[0].LogSource),
		LogType(alerts[0].LogType),
		Publishers(alerts[0].Publishers),
		RuleDescription(alerts[0].RuleDescription),
		SourceEntity(alerts[0].SourceEntity),
		SourceService(alerts[0].SourceService),
		Staged(anyStaged(alerts)),
	)
}

// computeCommon finds values common to all records
func computeCommon(records []shared.Record) map[string]interface{} {
	if len(records) == 0 {
		return make(map[string]interface{})
	}

	common := make(map[string]interface{})
	for key, val := range records[0] {
		allEqual := true
		for _, record := range records[1:] {
			if !reflect.DeepEqual(val, record[key]) {
				allEqual = false
				break
			}
		}
		if allEqual {
			common[key] = val
		}
	}
	return common
}

// getValueDiffs finds values in the records that are not in the common subset
func getValueDiffs(common map[string]interface{}, alerts []*Alert, records []shared.Record) map[string]interface{} {
	valueDiffs := make(map[string]interface{})
	for i, record := range records {
		diff := record.ComputeDiff(common)
		if len(diff) > 0 {
			valueDiffs[alerts[i].Created.Format(DATETIME_FORMAT)] = diff
		}
	}
	return valueDiffs
}

// anyStaged checks if any alert is staged
func anyStaged(alerts []*Alert) bool {
	for _, alert := range alerts {
		if alert.Staged {
			return true
		}
	}
	return false
}

// CreateFromDynamoRecord creates an alert from a DynamoDB record
func CreateFromDynamoRecord(record map[string]types.AttributeValue) (*Alert, error) {
	var err error
	a := new(Alert)

	err = attributevalue.UnmarshalMap(record, a)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal dynamodb record to alert: %w", err)
	}

	if createdStr, ok := record["Created"].(*types.AttributeValueMemberS); ok {
		a.Created, err = time.Parse(DATETIME_FORMAT, createdStr.Value)
		if err != nil {
			return nil, fmt.Errorf("failed to parse Created timestamp: %w", err)
		}
	}

	if dispatchedStr, ok := record["Dispatched"].(*types.AttributeValueMemberS); ok {
		dispatchedTime, err := time.Parse(DATETIME_FORMAT, dispatchedStr.Value)
		if err != nil {
			return nil, fmt.Errorf("failed to parse Dispatched timestamp: %w", err)
		}
		a.Dispatched = dispatchedTime
	}

	if recordStr, ok := record["Record"].(*types.AttributeValueMemberS); ok {
		err = json.Unmarshal([]byte(recordStr.Value), &a.Record)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal Record JSON: %w", err)
		}
	}

	return a, nil
}

// DynamoRecord converts the alert to a DynamoDB record
func (a *Alert) DynamoRecord() (map[string]types.AttributeValue, error) {
	item, err := attributevalue.MarshalMap(map[string]interface{}{
		"RuleName":        a.RuleName, // Partition Key
		"AlertID":         a.AlertID,  // Sort/Range Key
		"Attempts":        a.Attempts,
		"Cluster":         a.Cluster,
		"Context":         a.Context,
		"Created":         a.Created.Format(DATETIME_FORMAT),
		"Dispatched":      a.Dispatched.Format(DATETIME_FORMAT),
		"LogSource":       a.LogSource,
		"LogType":         a.LogType,
		"MergeByKeys":     a.MergeByKeys,
		"MergeWindow":     a.MergeWindow,
		"Outputs":         a.Outputs,
		"OutputsSent":     a.OutputsSent,
		"Publishers":      a.Publishers,
		"Record":          helpers.JsonCompact(a.Record),
		"RuleDescription": a.RuleDescription,
		"SourceEntity":    a.SourceEntity,
		"SourceService":   a.SourceService,
		"Staged":          a.Staged,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal alert to dynamodb record: %w", err)
	}
	return item, nil
}

// OutputDict converts the alert to a dictionary ready to send to an output
func (a *Alert) OutputDict() (map[string]interface{}, error) {
	output := map[string]interface{}{
		"cluster":          a.Cluster,
		"context":          a.Context,
		"created":          a.Created.Format(DATETIME_FORMAT),
		"id":               a.AlertID,
		"log_source":       a.LogSource,
		"log_type":         a.LogType,
		"outputs":          a.Outputs,
		"publishers":       a.Publishers,
		"record":           a.Record,
		"rule_description": a.RuleDescription,
		"rule_name":        a.RuleName,
		"source_entity":    a.SourceEntity,
		"source_service":   a.SourceService,
		"staged":           a.Staged,
	}
	return output, nil
}

func (a *Alert) DynamoKey() (map[string]types.AttributeValue, error) {
	key, err := attributevalue.MarshalMap(map[string]string{
		"RuleName": a.RuleName,
		"AlertID":  a.AlertID,
	})
	if err != nil {
		return nil, err
	}
	return key, nil
}

// String returns a simple representation of the alert
func (a *Alert) String() string {
	return fmt.Sprintf("<Alert %s triggered from %s>", a.AlertID, a.RuleName)
}

// FullString returns a detailed representation of the alert
func (a *Alert) FullString() string {
	dynamoRecord, err := a.DynamoRecord()
	if err != nil {
		return fmt.Sprintf("Error creating dynamo record: %v", err)
	}

	dynamoRecordJSON, err := json.MarshalIndent(dynamoRecord, "", "  ")
	if err != nil {
		return fmt.Sprintf("Error marshalling dynamo record: %v", err)
	}

	return string(dynamoRecordJSON)
}

// Less compares alerts by their creation time
func (a *Alert) Less(other *Alert) bool {
	return a.Created.Before(other.Created)
}

// CanMerge checks if two alerts can be merged together
func (a *Alert) CanMerge(other *Alert) bool {
	if !a.MergeEnabled() || !other.MergeEnabled() {
		return false
	}

	older, newer := a, other
	if newer.Created.Before(older.Created) {
		older, newer = newer, older
	}

	if newer.Created.After(older.Created.Add(time.Duration(older.MergeWindow) * time.Minute)) {
		return false
	}

	if !helpers.EqualStringSlices(a.MergeByKeys, other.MergeByKeys) {
		return false
	}

	for _, key := range a.MergeByKeys {
		if helpers.GetFirstKey(a.Record, key, "N/A") != helpers.GetFirstKey(other.Record, key, "N/A2") {
			return false
		}
	}

	return true
}

func (a *Alert) MergeEnabled() bool {
	return len(a.MergeByKeys) > 0 && a.MergeWindow > 0
}

func (a *Alert) RemainingOutputs(requiredOutputs []string) []string {
	var outputsToSendNow []string
	if a.MergeEnabled() {
		outputsToSendNow = helpers.Intersect(a.Outputs, requiredOutputs)
	} else {
		outputsToSendNow = a.Outputs
	}
	return helpers.Difference(outputsToSendNow, a.OutputsSent)
}
