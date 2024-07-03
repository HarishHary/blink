package shared

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"time"

	"github.com/harishhary/blink/src/helpers"
)

// AlertCreationError custom error for alert creation
type AlertCreationError struct {
	Message string
}

func (e *AlertCreationError) Error() string {
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
	Outputs         map[string]struct{}
	OutputsSent     map[string]struct{}
	Publishers      interface{}
	Record          Record
	RuleDescription string
	RuleName        string
	SourceEntity    string
	SourceService   string
	Staged          bool
}

// Constants for datetime format
const DATETIME_FORMAT = "2006-01-02T15:04:05.000Z"

// NewAlert creates a new Alert
func NewAlert(ruleName string, record Record, outputs map[string]struct{}, opts ...AlertOption) (*Alert, error) {
	alert := &Alert{
		AlertID:     helpers.GenerateUUID(),
		Created:     time.Now().UTC(),
		RuleName:    ruleName,
		Record:      record,
		Outputs:     outputs,
		Context:     make(map[string]interface{}),
		Publishers:  make(map[string]interface{}),
		OutputsSent: make(map[string]struct{}),
	}

	for _, opt := range opts {
		opt(alert)
	}

	if !(alert.RuleName != "" && len(alert.Outputs) > 0) {
		return nil, &AlertCreationError{Message: "Invalid Alert options"}
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
	cleanedRecords := make([]Record, len(alerts))
	for i, alert := range alerts {
		cleanedRecords[i] = cleanRecord(alert.Record, mergeKeys)
	}

	common := computeCommon(cleanedRecords)

	newRecord := Record{
		"AlertCount":      len(alerts),
		"AlertTimeFirst":  alerts[0].Created.Format(DATETIME_FORMAT),
		"AlertTimeLast":   alerts[len(alerts)-1].Created.Format(DATETIME_FORMAT),
		"MergedBy":        getMergedKeys(alerts[0].Record, mergeKeys),
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

// cleanRecord removes ignored keys from the record
func cleanRecord(record Record, ignoredKeys []string) Record {
	result := make(Record)
	for key, val := range record {
		if slices.Contains(ignoredKeys, key) {
			continue
		}
		if v, ok := val.(Record); ok {
			result[key] = cleanRecord(v, ignoredKeys)
		} else {
			result[key] = val
		}
	}
	return result
}

// computeCommon finds values common to all records
func computeCommon(records []Record) map[string]interface{} {
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

// getMergedKeys retrieves merge keys from a record
func getMergedKeys(record Record, keys []string) map[string]interface{} {
	mergeKeys := make(map[string]interface{})
	for _, key := range keys {
		mergeKeys[key] = helpers.GetFirstKey(record, key, "N/A")
	}
	return mergeKeys
}

// getValueDiffs finds values in the records that are not in the common subset
func getValueDiffs(common map[string]interface{}, alerts []*Alert, records []Record) map[string]interface{} {
	valueDiffs := make(map[string]interface{})
	for i, record := range records {
		diff := computeDiff(common, record)
		if len(diff) > 0 {
			valueDiffs[alerts[i].Created.Format(DATETIME_FORMAT)] = diff
		}
	}
	return valueDiffs
}

// computeDiff finds values in the record that are not in the common subset
func computeDiff(common map[string]interface{}, record Record) map[string]interface{} {
	diff := make(map[string]interface{})
	for key, val := range record {
		if commonVal, ok := common[key]; !ok || !reflect.DeepEqual(val, commonVal) {
			if v, ok := val.(map[string]interface{}); ok && reflect.TypeOf(commonVal).Kind() == reflect.Map {
				nestedDiff := computeDiff(commonVal.(map[string]interface{}), v)
				if len(nestedDiff) > 0 {
					diff[key] = nestedDiff
				}
			} else {
				diff[key] = val
			}
		}
	}
	return diff
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
