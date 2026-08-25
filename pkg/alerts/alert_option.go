package alerts

// AlertOptions sets one field on an alert as it is raised.
type AlertOptions func(*Alert)

// WithAttempts sets the alert's delivery attempts.
func WithAttempts(attempts int) AlertOptions {
	return func(a *Alert) {
		a.Attempts = attempts
	}
}

// WithCluster sets the alert's cluster.
func WithCluster(cluster string) AlertOptions {
	return func(a *Alert) {
		a.Cluster = cluster
	}
}

// WithLogSource sets the alert's log source.
func WithLogSource(logSource string) AlertOptions {
	return func(a *Alert) {
		a.LogSource = logSource
	}
}

// WithLogType sets the alert's log type.
func WithLogType(logType string) AlertOptions {
	return func(a *Alert) {
		a.LogType = logType
	}
}

// WithOutputsSent sets the outputs the alert has already been sent to.
func WithOutputsSent(outputsSent []string) AlertOptions {
	return func(a *Alert) {
		a.OutputsSent = outputsSent
	}
}

// WithSourceEntity sets the alert's source entity.
func WithSourceEntity(sourceEntity string) AlertOptions {
	return func(a *Alert) {
		a.SourceEntity = sourceEntity
	}
}

// WithSourceService sets the alert's source service.
func WithSourceService(sourceService string) AlertOptions {
	return func(a *Alert) {
		a.SourceService = sourceService
	}
}

// WithStaged sets whether the alert is staged.
func WithStaged(staged bool) AlertOptions {
	return func(a *Alert) {
		a.Staged = staged
	}
}
