package alerts

// AlertOptions defines the functional option type
type AlertOptions func(*Alert)

// Attempts sets the number of attempts for the alert
func WithAttempts(attempts int) AlertOptions {
	return func(a *Alert) {
		a.Attempts = attempts
	}
}

// Cluster sets the cluster for the alert
func WithCluster(cluster string) AlertOptions {
	return func(a *Alert) {
		a.Cluster = cluster
	}
}

// LogSource sets the log source for the alert
func WithLogSource(logSource string) AlertOptions {
	return func(a *Alert) {
		a.LogSource = logSource
	}
}

// LogType sets the log type for the alert
func WithLogType(logType string) AlertOptions {
	return func(a *Alert) {
		a.LogType = logType
	}
}

// OutputsSent sets the outputs sent for the alert
func WithOutputsSent(outputsSent []string) AlertOptions {
	return func(a *Alert) {
		a.OutputsSent = outputsSent
	}
}

// SourceEntity sets the source entity for the alert
func WithSourceEntity(sourceEntity string) AlertOptions {
	return func(a *Alert) {
		a.SourceEntity = sourceEntity
	}
}

// SourceService sets the source service for the alert
func WithSourceService(sourceService string) AlertOptions {
	return func(a *Alert) {
		a.SourceService = sourceService
	}
}

// SourceEntity sets the source entity for the alert
func WithStaged(staged bool) AlertOptions {
	return func(a *Alert) {
		a.Staged = staged
	}
}
