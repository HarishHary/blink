package alertmerger

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/harishhary/blink/src/shared/alerts"
	"github.com/harishhary/blink/src/shared/backends"
	"github.com/harishhary/blink/src/shared/helpers"
)

var logger = log.Default()

const (
	MAX_ALERTS_PER_GROUP          = 50
	MAX_LAMBDA_PAYLOAD_SIZE       = 126000
	ALERT_GENERATOR_DEFAULT_LIMIT = 5000
)

type AlertMergeGroup struct {
	alerts []*alerts.Alert
}

func NewAlertMergeGroup(a *alerts.Alert) *AlertMergeGroup {
	return &AlertMergeGroup{alerts: []*alerts.Alert{a}}
}

func (g *AlertMergeGroup) Add(a *alerts.Alert) bool {
	if len(g.alerts) >= MAX_ALERTS_PER_GROUP {
		return false
	}
	if a.CanMerge(g.alerts[0]) {
		g.alerts = append(g.alerts, a)
		return true
	}
	return false
}

type AlertMerger struct {
	alertProc           string
	alertProcTimeout    int
	lambdaClient        *lambda.Client
	alertGeneratorLimit int
	backend             backends.IBackend
}

var instance *AlertMerger

func GetInstance() *AlertMerger {
	if instance == nil {
		instance = NewAlertMerger()
	}
	return instance
}

func NewAlertMerger() *AlertMerger {
	ctx := context.Background()
	sdkConfig, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil
	}
	backend, err := backends.NewDynamoDBBackend(ctx, os.Getenv("ALERTS_TABLE"))
	if err != nil {
		return nil
	}
	return &AlertMerger{
		backend:             backend,
		alertProc:           os.Getenv("ALERT_PROCESSOR"),
		alertProcTimeout:    getEnvInt("ALERT_PROCESSOR_TIMEOUT_SEC", 60),
		lambdaClient:        lambda.NewFromConfig(sdkConfig),
		alertGeneratorLimit: 5000,
	}
}

func (am *AlertMerger) getAlertGenerator(ruleName string) chan *alerts.Alert {
	out := make(chan *alerts.Alert)
	go func() {
		defer close(out)
		generator := am.backend.GetAlertRecords(ruleName, am.alertProcTimeout)
		idx := 0
		for record := range generator {
			if idx >= am.alertGeneratorLimit {
				logger.Printf("Alert Merger reached alert limit of %d for rule \"%s\"", am.alertGeneratorLimit, ruleName)
				return
			}
			alert, err := am.backend.ToAlert(record)
			if err != nil {
				logger.Printf("Invalid alert record %s: %v", record, err)
				continue
			}
			out <- alert
			idx++
		}
	}()
	return out
}

func (am *AlertMerger) mergeGroups(alerts []*alerts.Alert) []*AlertMergeGroup {
	var mergeGroups []*AlertMergeGroup
	sort.Slice(alerts, func(i, j int) bool {
		return alerts[i].Created.Before(alerts[j].Created)
	})
	for _, alert := range alerts {
		added := false
		for _, group := range mergeGroups {
			if group.Add(alert) {
				added = true
				break
			}
		}
		if !added {
			if time.Now().Before(alert.Created.Add(time.Duration(alert.MergeWindow) * time.Minute)) {
				break
			}
			mergeGroups = append(mergeGroups, NewAlertMergeGroup(alert))
		}
	}
	return mergeGroups
}

func (am *AlertMerger) dispatchAlert(a *alerts.Alert) {
	a.Attempts++
	logger.Printf("Dispatching %s to %s (attempt %d)", a, am.alertProc, a.Attempts)
	// metrics.LogMetric(metrics.ALERT_MERGER_NAME, metrics.ALERT_ATTEMPTS, a.Attempts)

	dynamoRecord, _ := am.backend.ToRecord(a)
	recordPayload, _ := json.Marshal(dynamoRecord)

	payload := recordPayload
	record_key := a.RecordKey()
	if len(recordPayload) > MAX_LAMBDA_PAYLOAD_SIZE {
		payload, _ = json.Marshal(record_key)
	}

	ctx := context.Background()
	am.lambdaClient.Invoke(ctx, &lambda.InvokeInput{
		FunctionName:   aws.String(am.alertProc),
		InvocationType: types.InvocationTypeEvent,
		Payload:        payload,
		Qualifier:      aws.String("production"),
	})

	a.Dispatched = time.Now()
	am.backend.MarkAsDispatched(a)
}

func (am *AlertMerger) Dispatch() {
	var mergedAlerts []*alerts.Alert
	var alertsToDelete []*alerts.Alert

	for ruleName := range am.backend.RuleNamesGenerator() {
		var mergeEnabledAlerts []*alerts.Alert
		for alert := range am.getAlertGenerator(ruleName) {
			if len(alert.RemainingOutputs(helpers.GetRequiredOutputs())) > 0 {
				am.dispatchAlert(alert)
			} else if alert.MergeEnabled() {
				mergeEnabledAlerts = append(mergeEnabledAlerts, alert)
			} else {
				alertsToDelete = append(alertsToDelete, alert)
			}
		}

		for _, group := range am.mergeGroups(mergeEnabledAlerts) {

			newAlert, _ := alerts.Merge(group.alerts)
			logger.Printf("Merged %d alerts into a new alert with ID %s", len(group.alerts), newAlert.AlertID)
			mergedAlerts = append(mergedAlerts, newAlert)
			alertsToDelete = append(alertsToDelete, group.alerts...)
		}
	}

	if len(mergedAlerts) > 0 {
		am.backend.AddAlerts(mergedAlerts)
		for _, alert := range mergedAlerts {
			am.dispatchAlert(alert)
		}
	}

	if len(alertsToDelete) > 0 {
		var keys [][]string
		for _, alert := range alertsToDelete {
			keys = append(keys, []string{alert.RuleName, alert.AlertID})
		}
		am.backend.DeleteAlerts(keys)
	}
}

func Handler(ctx context.Context, event events.CloudWatchEvent) {
	NewAlertMerger().Dispatch()
}

func getEnvInt(key string, defaultValue int) int {
	if val, ok := os.LookupEnv(key); ok {
		intVal, err := strconv.Atoi(val)
		if err != nil {
			log.Printf("Error converting %s to int: %v", key, err)
			return defaultValue
		}
		return intVal
	}
	return defaultValue
}
