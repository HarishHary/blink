package alerts

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/rules"
	"github.com/harishhary/blink/pkg/scoring"
)

// Alert struct encapsulates a single alert and handles serialization
type Alert struct {
	AlertID     string
	Attempts    int
	Cluster     string
	Created     time.Time
	Dispatched  time.Time
	Event       events.Event
	Staged      bool
	OutputsSent []string

	LogSource string
	LogType   string

	SourceEntity  string
	SourceService string

	Confidence scoring.Confidence // coming from base rule but changed by tuning rules
	Severity   scoring.Severity   // coming from base rule but changed by asset tagging and dynamicSeverity

	// Add this field:
	Rule rules.IRule
}

// NewAlert creates a new Alert
func NewAlert(rule rules.IRule, event events.Event, optFns ...AlertOptions) (*Alert, errors.Error) {
	alert := &Alert{
		AlertID:  uuid.NewString(),
		Created:  time.Now().UTC(),
		Attempts: 0,
		Event:    event,
		Rule:     rule,
		Staged:   false,
	}
	for _, optFn := range optFns {
		optFn(alert)
	}
	return alert, nil
}

// Merge merges multiple alerts into a new merged alert
func Merge(alerts []*Alert) (*Alert, errors.Error) {
	if len(alerts) == 0 {
		return nil, errors.New("no alerts to merge")
	}

	sort.Slice(alerts, func(i, j int) bool {
		return alerts[i].Created.Before(alerts[j].Created)
	})

	mergeKeys := alerts[0].Rule.MergeByKeys()
	cleanedEvents := make([]events.Event, len(alerts))
	for i, alert := range alerts {
		cleanedEvents[i] = alert.Event.CleanEvent(mergeKeys)
	}

	common := computeCommon(cleanedEvents)

	newEvent := events.Event{
		"AlertCount":      len(alerts),
		"AlertTimeFirst":  alerts[0].Created.Format(helpers.DATETIME_FORMAT),
		"AlertTimeLast":   alerts[len(alerts)-1].Created.Format(helpers.DATETIME_FORMAT),
		"MergedBy":        alerts[0].Event.GetMergedKeys(mergeKeys),
		"OtherCommonKeys": common,
		"ValueDiffs":      getValueDiffs(common, alerts, cleanedEvents),
	}

	return NewAlert(
		alerts[0].Rule,
		newEvent,
		WithCluster(alerts[0].Cluster),
		WithLogSource(alerts[0].LogSource),
		WithLogType(alerts[0].LogType),
		WithSourceEntity(alerts[0].SourceEntity),
		WithSourceService(alerts[0].SourceService),
		WithStaged(anyStaged(alerts)),
	)
}

// computeCommon finds values common to all records
func computeCommon(events []events.Event) map[string]any {
	if len(events) == 0 {
		return make(map[string]any)
	}

	common := make(map[string]any)
	for key, val := range events[0] {
		allEqual := true
		for _, event := range events[1:] {
			if !reflect.DeepEqual(val, event[key]) {
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
func getValueDiffs(common map[string]any, alerts []*Alert, events []events.Event) map[string]any {
	valueDiffs := make(map[string]any)
	for i, event := range events {
		diff := event.ComputeDiff(common)
		if len(diff) > 0 {
			valueDiffs[alerts[i].Created.Format(helpers.DATETIME_FORMAT)] = diff
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

// OutputDict converts the alert to a dictionary ready to send to an output
func (a *Alert) OutputDict() map[string]any {
	output := map[string]any{
		"cluster":          a.Cluster,
		"created":          a.Created.Format(helpers.DATETIME_FORMAT),
		"id":               a.AlertID,
		"log_source":       a.LogSource,
		"log_type":         a.LogType,
		"outputs":          a.Rule.Dispatchers(),
		"formatters":       a.Rule.Formatters(),
		"event":            a.Event,
		"rule_description": a.Rule.Description(),
		"rule_name":        a.Rule.Name(),
		"source_entity":    a.SourceEntity,
		"source_service":   a.SourceService,
		"staged":           a.Staged,
	}
	return output
}

// String returns a simple representation of the alert
func (a *Alert) String() string {
	return fmt.Sprintf("<Alert %s triggered from %s>", a.AlertID, a.Rule.Name())
}

// FullString returns a detailed representation of the alert
func (a *Alert) FullString() (string, errors.Error) {
	recordJSON, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return "", errors.NewF("error marshalling record: %s", err)
	}

	return string(recordJSON), nil
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

	if newer.Created.After(older.Created.Add(time.Duration(older.Rule.MergeWindowMins()) * time.Minute)) {
		return false
	}

	if !helpers.EqualStringSlices(a.Rule.MergeByKeys(), other.Rule.MergeByKeys()) {
		return false
	}

	for _, key := range a.Rule.MergeByKeys() {
		if a.Event.GetFirstKey(key, "n/a") != other.Event.GetFirstKey(key, "n/a2") {
			return false
		}
	}

	return true
}

func (a *Alert) MergeEnabled() bool {
	return len(a.Rule.MergeByKeys()) > 0 && a.Rule.MergeWindowMins() > 0
}

func (a *Alert) RemainingOutputs(requiredOutputs []string) []string {
	var outputsToSendNow []string
	if a.MergeEnabled() {
		outputsToSendNow = helpers.Intersect(a.Rule.Dispatchers(), requiredOutputs)
	} else {
		outputsToSendNow = a.Rule.Dispatchers()
	}
	return helpers.Difference(outputsToSendNow, a.OutputsSent)
}

func (a *Alert) RecordKey() map[string]string {
	key := map[string]string{
		"RuleName": a.Rule.Name(),
		"AlertID":  a.AlertID,
	}
	return key
}

func (a *Alert) Signal() bool {
	if a.Rule.Signal() && a.Rule.SignalThreshold() <= a.Confidence {
		return false
	}
	return true
}

func (a *Alert) RiskScore() scoring.RiskScore {
	return scoring.ComputeRiskScore(a.Confidence, a.Severity)
}

func (a *Alert) SignalType() scoring.SignalType {
	return scoring.ComputeSignalType(a.Confidence)
}
