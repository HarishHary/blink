package alerts

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/rules"
	"github.com/harishhary/blink/pkg/scoring"
)

// Alert wraps a detection Event with its rule reference, scoring, and pipeline metadata as it flows through the stages.
type Alert struct {
	Id                 string
	Attempts           int
	Cluster            string
	Created            time.Time
	Dispatched         time.Time
	Event              events.Event
	Staged             bool
	OutputsSent        []string
	EnrichmentsApplied []string

	LogSource string
	LogType   string

	SourceEntity  string
	SourceService string

	Confidence scoring.Confidence
	Severity   scoring.Severity

	Rule                *rules.RuleMetadata
	OverrideMergeByKeys []string
}

// Clone returns an alert copy with independently owned mutable fields.
func (a *Alert) Clone() *Alert {
	clone := *a
	clone.Event = a.Event.Clone()
	clone.OutputsSent = append([]string(nil), a.OutputsSent...)
	clone.EnrichmentsApplied = append([]string(nil), a.EnrichmentsApplied...)
	clone.OverrideMergeByKeys = append([]string(nil), a.OverrideMergeByKeys...)
	return &clone
}

// CloneBatch returns alert copies with deeply cloned Events, preserving input order.
func CloneBatch(in []*Alert) []*Alert {
	out := make([]*Alert, len(in))
	for i, alert := range in {
		out[i] = alert.Clone()
	}
	return out
}

// MergeByKeys returns the effective merge keys; the plugin's AlertMergeByKeys override takes precedence over the YAML value.
func (a *Alert) MergeByKeys() []string {
	if len(a.OverrideMergeByKeys) > 0 {
		return a.OverrideMergeByKeys
	}
	if a.Rule == nil {
		return nil
	}
	return a.Rule.MergeByKeys
}

// Creates a new Alert
func NewAlert(rule *rules.RuleMetadata, event events.Event, optFns ...AlertOptions) (*Alert, errors.Error) {
	alert := &Alert{
		Id:         uuid.NewString(),
		Created:    time.Now().UTC(),
		Attempts:   0,
		Event:      event,
		Rule:       rule,
		Staged:     false,
		Severity:   rule.Severity,
		Confidence: rule.Confidence,
	}
	for _, optFn := range optFns {
		optFn(alert)
	}
	return alert, nil
}

// Merges multiple alerts into a new merged alert
func Merge(alerts []*Alert) (*Alert, errors.Error) {
	if len(alerts) == 0 {
		return nil, errors.New("no alerts to merge")
	}

	sort.Slice(alerts, func(i, j int) bool {
		return alerts[i].Created.Before(alerts[j].Created)
	})

	mergeKeys := alerts[0].MergeByKeys()
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

	merged, err := NewAlert(
		alerts[0].Rule,
		newEvent,
		WithCluster(alerts[0].Cluster),
		WithLogSource(alerts[0].LogSource),
		WithLogType(alerts[0].LogType),
		WithSourceEntity(alerts[0].SourceEntity),
		WithSourceService(alerts[0].SourceService),
		WithStaged(anyStaged(alerts)),
	)
	if err != nil {
		return nil, err
	}
	merged.OverrideMergeByKeys = append([]string(nil), alerts[0].OverrideMergeByKeys...)
	merged.Severity = alerts[0].Severity
	merged.Confidence = alerts[0].Confidence
	for _, alert := range alerts {
		if alert.Severity > merged.Severity {
			merged.Severity = alert.Severity
		}
		if alert.Confidence > merged.Confidence {
			merged.Confidence = alert.Confidence
		}
	}
	return merged, nil
}

// Finds values common to all records
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

// Finds values in the records that are not in the common subset
func getValueDiffs(common map[string]any, alerts []*Alert, events []events.Event) map[string]any {
	valueDiffs := make(map[string]any)
	for i, event := range events {
		diff := event.ComputeDiff(common)
		if len(diff) > 0 {
			key := fmt.Sprintf("%s|%s|%d", alerts[i].Created.Format(time.RFC3339Nano), alerts[i].Id, i)
			valueDiffs[key] = diff
		}
	}
	return valueDiffs
}

// Checks if any alert is staged
func anyStaged(alerts []*Alert) bool {
	for _, alert := range alerts {
		if alert.Staged {
			return true
		}
	}
	return false
}

// Converts the alert to a dictionary ready to send to an output
func (a *Alert) OutputDict() map[string]any {
	output := map[string]any{
		"cluster":          a.Cluster,
		"created":          a.Created.Format(helpers.DATETIME_FORMAT),
		"id":               a.Id,
		"log_source":       a.LogSource,
		"log_type":         a.LogType,
		"outputs":          a.Rule.Dispatchers,
		"formatters":       a.Rule.Formatters,
		"event":            a.Event,
		"rule_description": a.Rule.Description,
		"rule_name":        a.Rule.Name,
		"source_entity":    a.SourceEntity,
		"source_service":   a.SourceService,
		"staged":           a.Staged,
	}
	return output
}

// Returns a simple representation of the alert
func (a *Alert) String() string {
	return fmt.Sprintf("Alert %s triggered from %s", a.Id, a.Rule.Name)
}

// Returns a detailed representation of the alert
func (a *Alert) FullString() (string, errors.Error) {
	recordJSON, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return "", errors.NewF("error marshalling record: %s", err)
	}
	return string(recordJSON), nil
}

// Compares alerts by their creation time
func (a *Alert) Less(other *Alert) bool {
	return a.Created.Before(other.Created)
}

// Checks if two alerts can be merged together
func (a *Alert) CanMerge(other *Alert) bool {
	if other == nil || !a.MergeEnabled() || !other.MergeEnabled() {
		return false
	}
	if ruleIdentity(a.Rule) != ruleIdentity(other.Rule) || a.Rule.Version != other.Rule.Version {
		return false
	}

	older, newer := a, other
	if newer.Created.Before(older.Created) {
		older, newer = newer, older
	}

	if newer.Created.After(older.Created.Add(older.Rule.MergeWindowMins())) {
		return false
	}

	if !helpers.EqualStringSlices(a.MergeByKeys(), other.MergeByKeys()) {
		return false
	}

	for _, key := range a.MergeByKeys() {
		left := a.Event.GetKeys(key, 1)
		right := other.Event.GetKeys(key, 1)
		if len(left) != 1 || len(right) != 1 || !reflect.DeepEqual(left[0], right[0]) {
			return false
		}
	}

	return true
}

// MergeEnabled reports whether this alert is eligible for merging (has merge keys and a positive window).
func (a *Alert) MergeEnabled() bool {
	return a != nil && a.Rule != nil && a.Event != nil && len(a.MergeByKeys()) > 0 && a.Rule.MergeWindowMins() > 0
}

// MergePartitionKey returns a tenant- and rule-version-scoped key with typed merge values.
func (a *Alert) MergePartitionKey() string {
	keys := append([]string(nil), a.MergeByKeys()...)
	sort.Strings(keys)
	ruleID := ""
	version := ""
	if a.Rule != nil {
		ruleID = ruleIdentity(a.Rule)
		version = a.Rule.Version
	}
	tenant := pools.NormalizeRolloutKey(nil)
	if a.Event != nil {
		tenant = pools.NormalizeRolloutKey(a.Event["tenant_id"])
	}
	parts := []string{
		"tenant=" + mergeKeyValue(tenant, true),
		"rule=" + mergeKeyValue(ruleID, true),
		"version=" + mergeKeyValue(version, true),
	}
	for _, k := range keys {
		values := a.Event.GetKeys(k, 1)
		var value any
		if len(values) == 1 {
			value = values[0]
		}
		parts = append(parts, mergeKeyValue(k, true)+"="+mergeKeyValue(value, len(values) == 1))
	}
	return strings.Join(parts, "|")
}

func ruleIdentity(rule *rules.RuleMetadata) string {
	if rule == nil {
		return ""
	}
	if rule.Id != "" {
		return rule.Id
	}
	return rule.Name
}

func mergeKeyValue(value any, present bool) string {
	if !present {
		return "<missing>"
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%T:%#v", value, value)
	}
	return string(payload)
}

// RemainingOutputs returns the alert's dispatchers not yet sent - narrowed to requiredOutputs when merge is enabled.
func (a *Alert) RemainingOutputs(requiredOutputs []string) []string {
	var outputsToSendNow []string
	if a.MergeEnabled() {
		outputsToSendNow = helpers.Intersect(a.Rule.Dispatchers, requiredOutputs)
	} else {
		outputsToSendNow = a.Rule.Dispatchers
	}
	return helpers.Difference(outputsToSendNow, a.OutputsSent)
}

// RecordKey returns the {RuleName, AlertID} key identifying this alert in the store.
func (a *Alert) RecordKey() map[string]string {
	key := map[string]string{
		"RuleName": a.Rule.Name,
		"AlertID":  a.Id,
	}
	return key
}

// Signal reports whether this alert qualifies as a signal (the rule opts in and confidence meets its threshold).
func (a *Alert) Signal() bool {
	return a.Rule.Signal && a.Rule.SignalThreshold <= a.Confidence
}

// RiskScore computes the alert's risk score from its confidence and severity.
func (a *Alert) RiskScore() scoring.RiskScore {
	return scoring.ComputeRiskScore(a.Confidence, a.Severity)
}

// SignalType classifies the alert's signal from its confidence.
func (a *Alert) SignalType() scoring.SignalType {
	return scoring.ComputeSignalType(a.Confidence)
}
