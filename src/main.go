package blink

import (
	"fmt"
	"plugin"
)

// var allrules []rules.DetectionRule

// func main() {
// 	beam.Init()

// 	ctx := context.Background()

// 	p := beam.NewPipeline()
// 	s := p.Root()

// 	// Kafka configuration for SigninLogs
// 	kafkaSigninConfig := kafkaio.ReadConfig{
// 		BootstrapServers: "localhost:9092",
// 		Topics:           []string{"signinlogs"},
// 		ConsumerConfig:   map[string]string{"group.id": "beam-group-signin"},
// 	}

// 	// Kafka configuration for AuditLogs
// 	kafkaAuditConfig := kafkaio.ReadConfig{
// 		BootstrapServers: "localhost:9092",
// 		Topics:           []string{"auditlogs"},
// 		ConsumerConfig:   map[string]string{"group.id": "beam-group-audit"},
// 	}

// 	// Read SigninLogs from Kafka
// 	signinEvents := kafkaio.Read(s, kafkaSigninConfig)

// 	// Read AuditLogs from Kafka
// 	auditEvents := kafkaio.Read(s, kafkaAuditConfig)

// 	// Parse SigninLogs
// 	parsedSigninEvents := beam.ParDo(s, parseSigninEventFn, signinEvents)

// 	// Parse AuditLogs
// 	parsedAuditEvents := beam.ParDo(s, parseAuditEventFn, auditEvents)

// 	// Apply windowing
// 	windowedSigninEvents := beam.WindowInto(s, window.NewFixedWindows(24*time.Hour), parsedSigninEvents)
// 	windowedAuditEvents := beam.WindowInto(s, window.NewFixedWindows(24*time.Hour), parsedAuditEvents)

// 	// Load rules dynamically
// 	loadRules()

// 	// Apply detection rules
// 	detectedThreats := applyDetectionRules(s, windowedSigninEvents, windowedAuditEvents)

// 	// Write detected threats to output (e.g., another Kafka topic, a database, etc.)
// 	kafkaio.Write(s, "localhost:9092", "threats", detectedThreats)
// 	_, err := prism.Execute(ctx, p)
// 	if err != nil {
// 		log.Fatalf(ctx, "Failed to execute job: %v", err)
// 	}
// }

// func parseSigninEventFn(ctx context.Context, record kafkaio.ReadMessage, emit func(beam.EventTime, SigninLog)) {
// 	var signinLog SigninLog
// 	if err := json.Unmarshal(record.Value, &signinLog); err != nil {
// 		log.Errorf(ctx, "Failed to parse signin log: %v", err)
// 		return
// 	}
// 	emit(beam.EventTime(signinLog.TimeGenerated), signinLog)
// }

// func parseAuditEventFn(ctx context.Context, record kafkaio.ReadMessage, emit func(beam.EventTime, AuditLog)) {
// 	var auditLog AuditLog
// 	if err := json.Unmarshal(record.Value, &auditLog); err != nil {
// 		log.Errorf(ctx, "Failed to parse audit log: %v", err)
// 		return
// 	}
// 	emit(beam.EventTime(auditLog.TimeGenerated), auditLog)
// }

// func loadRules() {
// 	// Here, you would load the rules from plugins or statically defined in code
// 	allrules = []rules.DetectionRule{
// 		RiskySigninAndSSPRRule,
// 	}
// }

// func applyDetectionRules(s beam.Scope, signinLogs, auditLogs beam.PCollection) beam.PCollection {
// 	// Key PCollections by UserPrincipalName
// 	keyedSigninLogs := beam.ParDo(s, func(signinLog SigninLog) (string, SigninLog) {
// 		return signinLog.UserPrincipalName, signinLog
// 	}, signinLogs)

// 	keyedAuditLogs := beam.ParDo(s, func(auditLog AuditLog) (string, AuditLog) {
// 		return auditLog.UserPrincipalName, auditLog
// 	}, auditLogs)

// 	// Group by key (UserPrincipalName) within each window
// 	groupedSigninLogs := beam.GroupByKey(s, keyedSigninLogs)
// 	groupedAuditLogs := beam.GroupByKey(s, keyedAuditLogs)

// 	// Join the grouped PCollections on UserPrincipalName
// 	joinedLogs := beam.CoGroupByKey(s, groupedSigninLogs, groupedAuditLogs)

// 	// Apply the correlation logic
// 	correlatedEvents := beam.ParDo(s, func(userPrincipalName string, signinLogsIter, auditLogsIter func(*SigninLog) bool, emit func(string)) {
// 		var signinLog SigninLog
// 		var auditLog AuditLog
// 		signinLogs := []SigninLog{}
// 		auditLogs := []AuditLog{}

// 		for signinLogsIter(&signinLog) {
// 			signinLogs = append(signinLogs, signinLog)
// 		}
// 		for auditLogsIter(&auditLog) {
// 			auditLogs = append(auditLogs, auditLog)
// 		}

// 		events := appendEvents(signinLogs, auditLogs)

// 		for _, rule := range allrules {
// 			if rule.Condition(events) {
// 				severity := rule.ApplyTuningRules(events[0]) // Assuming severity is calculated based on the first event

// 				if err := rule.ApplyEnrichments(events); err != nil {
// 					log.Errorf(ctx, "Failed to apply enrichments: %v", err)
// 					return
// 				}

// 				emit(fmt.Sprintf("Incident detected for user %s: %s - Severity: %d", userPrincipalName, rule.Description, severity))
// 			}
// 		}
// 	}, joinedLogs)

// 	return correlatedEvents
// }

// func appendEvents(signinLogs []SigninLog, auditLogs []AuditLog) []events.Event {
// 	events := []events.Event{}
// 	for _, signinLog := range signinLogs {
// 		events = append(events, events.Event{
// 			UserID:          signinLog.UserPrincipalName,
// 			EventType:       "SigninLog",
// 			Timestamp:       signinLog.TimeGenerated,
// 			Location:        signinLog.Location,
// 			IP:              signinLog.IPAddress,
// 			Status:          signinLog.ResultDescription,
// 			FailedAttempts:  signinLog.FailedAttempts,
// 			DataTransferred: signinLog.DataTransferred,
// 			GeoLocation:     signinLog.GeoLocation,
// 			User:            signinLog.User,
// 		})
// 	}
// 	for _, auditLog := range auditLogs {
// 		events = append(events, events.Event{
// 			UserID:    auditLog.UserPrincipalName,
// 			EventType: "AuditLog",
// 			Timestamp: auditLog.TimeGenerated,
// 			User:      auditLog.User,
// 		})
// 	}
// 	return events
// }

// func within24Hours(t1, t2 time.Time) bool {
// 	return t1.Sub(t2).Hours() <= 24 && t2.Sub(t1).Hours() <= 24
// }

func LoadPlugins[T any](paths []string) ([]T, error) {
	var plugins []T
	for _, path := range paths {
		p, err := plugin.Open(path)
		if err != nil {
			return nil, err
		}
		sym, err := p.Lookup("Plugin")
		if err != nil {
			return nil, err
		}
		pluginInstance, ok := sym.(T)
		if !ok {
			return nil, fmt.Errorf("invalid type for plugin %s", path)
		}
		plugins = append(plugins, pluginInstance)
	}
	return plugins, nil
}
