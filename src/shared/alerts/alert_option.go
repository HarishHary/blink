package alerts

import (
	"time"
)

// AlertOptions defines the functional option type
type AlertOptions func(*Alert)

// Attempts sets the number of attempts for the alert
func Attempts(attempts int) AlertOptions {
	return func(a *Alert) {
		a.Attempts = attempts
	}
}

// Cluster sets the cluster for the alert
func Cluster(cluster string) AlertOptions {
	return func(a *Alert) {
		a.Cluster = cluster
	}
}

// Created sets the creation time for the alert
func Created(created time.Time) AlertOptions {
	return func(a *Alert) {
		a.Created = created
	}
}

// Dispatched sets the dispatched time for the alert
func Dispatched(dispatched time.Time) AlertOptions {
	return func(a *Alert) {
		a.Dispatched = dispatched
	}
}

// LogSource sets the log source for the alert
func LogSource(logSource string) AlertOptions {
	return func(a *Alert) {
		a.LogSource = logSource
	}
}

// LogType sets the log type for the alert
func LogType(logType string) AlertOptions {
	return func(a *Alert) {
		a.LogType = logType
	}
}

// MergeByKeys sets the merge by keys for the alert
func MergeByKeys(mergeByKeys []string) AlertOptions {
	return func(a *Alert) {
		a.MergeByKeys = mergeByKeys
	}
}

// MergeWindow sets the merge window for the alert
func MergeWindow(mergeWindow time.Duration) AlertOptions {
	return func(a *Alert) {
		a.MergeWindow = mergeWindow
	}
}

// OutputsSent sets the outputs sent for the alert
func OutputsSent(outputsSent []string) AlertOptions {
	return func(a *Alert) {
		a.OutputsSent = outputsSent
	}
}

// Formatters sets the formatters for the alert
func Formatters(formatters []string) AlertOptions {
	return func(a *Alert) {
		a.Formatters = formatters
	}
}

// RuleDescription sets the rule description for the alert
func RuleDescription(ruleDescription string) AlertOptions {
	return func(a *Alert) {
		a.RuleDescription = ruleDescription
	}
}

// SourceEntity sets the source entity for the alert
func SourceEntity(sourceEntity string) AlertOptions {
	return func(a *Alert) {
		a.SourceEntity = sourceEntity
	}
}

// SourceService sets the source service for the alert
func SourceService(sourceService string) AlertOptions {
	return func(a *Alert) {
		a.SourceService = sourceService
	}
}

// Staged sets the staged flag for the alert
func Staged(staged bool) AlertOptions {
	return func(a *Alert) {
		a.Staged = staged
	}
}
