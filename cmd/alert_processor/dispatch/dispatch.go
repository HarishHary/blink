package dispatch

import (
	"log"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/harishhary/blink/cmd/alert_processor/internal/message"
	"github.com/harishhary/blink/internal/backends"
	"github.com/harishhary/blink/internal/backends/dynamodb"
	"github.com/harishhary/blink/internal/configuration"
	"github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/dispatchers"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/formatters"
)

const BACKOFF_MAX_TRIES = 5

type AlertProcessor struct {
	backend      backends.IBackend
	lambdaClient *lambda.Client
}

type DispatcherService struct {
	context.ServiceContext
	syncMessages       messaging.MessageQueue
	dispatcherMessages messaging.MessageQueue
	backend            backends.IBackend
}

func New() *DispatcherService {
	serviceContext := context.New("BLINK-ALERT-PROCESSOR - DISPATCHER")
	if err := configuration.LoadFromEnvironment(&serviceContext); err != nil {
		log.Fatalln(err)
	}
	serviceContext.Logger = logger.New(serviceContext.Name(), "dev")

	dynamodb, err := dynamodb.NewDynamoDBBackend(os.Getenv("ALERTS_TABLE"))
	if err != nil {
		log.Fatalln(err)
	}
	return &DispatcherService{
		ServiceContext:     serviceContext,
		syncMessages:       serviceContext.Messages().Subscribe(message.SyncService, false),
		dispatcherMessages: serviceContext.Messages().Subscribe(message.DispatcherService, true),
		backend:            dynamodb,
	}
}

func (service *DispatcherService) Run() errors.Error {
	service.Info("getting dispatchers...")
	formatters := formatters.GetFormatterRepository()
	go func() {
		recv := func() messaging.Message {
			msg := <-service.syncMessages
			service.Debug("received message: '%v'", msg)
			return msg
		}

		for {
			newMessage := recv()
			service.Debug("recording new message: '%v'", newMessage)
			formatters.Record(newMessage)
		}
	}()

	for {
		newMessage := <-service.dispatcherMessages
		newAlert, ok := newMessage.(alerts.AlertMessage)
		if !ok {
			service.ErrorF("invalid message type")
			continue
		}

		alert := newAlert.Alert
		service.Info("applying dispatchers for alert %s", alert.AlertID)
		if err := applyDispatchers(service, &alert); err != nil {
			service.Error(err)
		}
	}
}

// createDispatcher creates a dispatcher for the given output.
func (ap *DispatcherService) createDispatcher(output string) (dispatchers.IDispatcher, error) {
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
		return nil, errors.NewE(err)
	}
	return dispatcher, nil
}

// dispatch simulates dispatching the alert.
func dispatch(dispatcher *dispatchers.IDispatcher, alert *alerts.Alert, output string) bool {
	return true // Placeholder for actual dispatch logic
}

// sendToOutputs sends an alert to each remaining output.
func (ap *DispatcherService) sendToOutputs(alert *alerts.Alert) map[string]bool {
	result := make(map[string]bool)
	for _, output := range alert.RemainingOutputs(nil) {
		dispatcher, err := ap.createDispatcher(output)
		if err != nil {
			log.Printf("Failed to create dispatcher for output %s: %v", output, err)
			result[output] = false
			continue
		}
		result[output], err = dispatcher.Dispatch(*alert)
		if err != nil {
			log.Printf("Failed to dispatch alert to output %s: %v", output, err)
			continue
		}
	}
	alert.OutputsSent = filterSuccessfulOutputs(result)
	return result
}

// updateTable updates the alerts table based on the results of the outputs.
func (ap *DispatcherService) updateTable(alert *alerts.Alert, outputResults map[string]bool) {
	if len(outputResults) == 0 {
		return
	}

	if allOutputsSuccessful(outputResults) && !alert.MergeEnabled() {
		var alerts = []*alerts.Alert{alert}
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

func (ap *DispatcherService) retrieveAlertRecord(event map[string]any) (backends.Record, error) {
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

func applyDispatchers(service *DispatcherService, alert *alerts.Alert) errors.Error {
	dispatcherRepository := dispatchers.GetDispatcherRepository()
	formatterRepository := formatters.GetFormatterRepository()
	for _, dispatcher := range alert.Rule.Dispatchers() {
		dispatcher, err := dispatcherRepository.GetDispatcher(dispatcher)
		if err != nil {
			service.Error(err)
			continue
		}
		service.Info("applying dispatcher %s for alert %s", dispatcher.Name(), alert.AlertID)

		_ = make(map[string]any) // TODO: Check formattedMap
		for _, formatter := range alert.Rule.Formatters() {
			formatter, err := formatterRepository.Get(formatter)
			if err != nil {
				service.Error(err)
				continue
			}
			if !formatter.Enabled() {
				service.Info("disabled formatter %s therefore skipping", formatter.Name())
				continue
			}
			service.Info("applying formatter %s for alert %s", formatter.Name(), alert.AlertID)
			_, err = formatter.Format(*alert) // TODO: Check formattedMap
			if err != nil {
				service.Error(err)
				continue
			}
		}
		_, err = dispatcher.Dispatch(*alert)
		if err != nil {
			service.Error(err)
		}
	}
	return nil
}
