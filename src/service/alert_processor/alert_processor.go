package alertprocessor

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/harishhary/blink/src/shared/alerts"
	"github.com/harishhary/blink/src/shared/backends"
	"github.com/harishhary/blink/src/shared/dispatchers"
)

var logger = log.Default()

const BACKOFF_MAX_TRIES = 5

var instance *AlertProcessor

type AlertProcessor struct {
	backend      backends.IBackend
	lambdaClient *lambda.Client
}

func GetInstance() *AlertProcessor {
	if instance == nil {
		instance = NewAlertProcessor()
	}
	return instance
}

func NewAlertProcessor() *AlertProcessor {
	ctx := context.Background()
	sdkConfig, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil
	}
	backend, err := backends.NewDynamoDBBackend(ctx, os.Getenv("ALERTS_TABLE"))
	if err != nil {
		return nil
	}
	return &AlertProcessor{
		backend:      backend,
		lambdaClient: lambda.NewFromConfig(sdkConfig),
	}
}

// createDispatcher creates a dispatcher for the given output.
func (ap *AlertProcessor) createDispatcher(output string) (*dispatchers.IDispatcher, error) {
	parts := strings.Split(output, ":")
	if len(parts) != 2 {
		return nil, errors.New("improperly formatted output")
	}

	service, descriptor := parts[0], parts[1]
	serviceConfig, ok := ap.config[service]
	if !ok || serviceConfig.(map[string]any)[descriptor] == nil {
		return nil, errors.New("output does not exist")
	}

	dispatcher, err := dispatchers.GetDispatcherRepository().GetDispatcher(service)
	if err != nil {
		return nil, errors.New("issue creating dispatcher...")
	}
	return &dispatcher, nil
}

func createDispatcherInstance(service string, config map[string]any) (*dispatchers.IDispatcher, error) {
	return nil, nil
}

// dispatch simulates dispatching the alert.
func dispatch(dispatcher *dispatchers.IDispatcher, alert *alerts.Alert, output string) bool {
	return true // Placeholder for actual dispatch logic
}

// sendToOutputs sends an alert to each remaining output.
func (ap *AlertProcessor) sendToOutputs(alert *alerts.Alert) map[string]bool {
	result := make(map[string]bool)
	for _, output := range alert.RemainingOutputs(nil) {
		dispatcher, err := ap.createDispatcher(output)
		if err != nil {
			log.Printf("Failed to create dispatcher for output %s: %v", output, err)
			result[output] = false
			continue
		}
		result[output] = dispatch(dispatcher, alert, output)
	}
	alert.OutputsSent = filterSuccessfulOutputs(result)
	return result
}

// updateTable updates the alerts table based on the results of the outputs.
func (ap *AlertProcessor) updateTable(alert *alerts.Alert, outputResults map[string]bool) {
	if len(outputResults) == 0 {
		return
	}

	if allOutputsSuccessful(outputResults) && !alert.MergeEnabled() {
		var alerts []*alerts.Alert
		alerts = append(alerts, alert)
		ap.backend.DeleteAlerts(alerts)
	} else if anyOutputSuccessful(outputResults) {
		ap.backend.UpdateSentOutputs(alert)
	}
}

// allOutputsSuccessful checks if all outputs were successful.
func allOutputsSuccessful(outputResults map[string]bool) bool {
	for _, success := range outputResults {
		if !success {
			return false
		}
	}
	return true
}

// anyOutputSuccessful checks if any output was successful.
func anyOutputSuccessful(outputResults map[string]bool) bool {
	for _, success := range outputResults {
		if success {
			return true
		}
	}
	return false
}

// filterSuccessfulOutputs filters and returns only the successful outputs.
func filterSuccessfulOutputs(outputResults map[string]bool) []string {
	var outputs []string
	for output, success := range outputResults {
		if success {
			outputs = append(outputs, output)
		}
	}
	return outputs
}

func (ap *AlertProcessor) retrieveAlertRecord(event map[string]any) (backends.Record, error) {
	if alertID, ok := event["AlertID"].(string); ok {
		ruleName, ok := event["RuleName"].(string)
		if !ok {
			return nil, errors.New("missing RuleName in event")
		}
		log.Printf("Retrieving alert with RuleName: %s and AlertID: %s", ruleName, alertID)
		return ap.backend.GetAlertRecord(ruleName, alertID)
	}
	return event, nil
}
