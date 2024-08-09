// rule engine service
Match(event events.Event) (bool, errors.Error)
Evaluate(event events.Event) (bool, errors.Error)

// need to have process malfunctions between rule engine and alert engine

// alert engine service
Enrich(alert *alerts.Alert) errors.Error // can have blocking functions => implement retry logic, and try later queue
Tune(alert alerts.Alert) (bool, errors.Error)

// need to have process malfunctions between alert engine and alert processor

// alert processor service
Format(alert alerts.Alert) (map[string]any, errors.Error) // done by the processor service
Dispatch(alert alerts.Alert) (bool, errors.Error) // done by the processor service => implement retry logic, and try later queue

// need to add docs
// need to add tests
// need to add CI/CD
// need to add a way to test a rule with the local rule engine
// need to add a way to test an alert with the local alert engine
// need to add a way to test an alert with the local alert processor
// need to implement asset tagging
// need to implement global tuning rules
// need to implement VRL service with VRL rules
// need to implement signal type rules and correlation rules
// need to implement graph based rule engine with FalconHound
// need to implement UI
// need a way to load rules from metadata in yaml + files