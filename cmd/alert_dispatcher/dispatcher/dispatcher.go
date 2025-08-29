package dispatcher

import (
	"log"

	"github.com/harishhary/blink/cmd/alert_dispatcher/internal/message"
	"github.com/harishhary/blink/internal/configuration"
	"github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/dispatchers"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/pkg/alerts"
)

// DispatcherService dispatches formatted alerts to downstream systems and publishes to merger.
type DispatcherService struct {
	context.ServiceContext
	dispatcherMessages messaging.MessageQueue
}

// New constructs an alert dispatcher service.
func New() *DispatcherService {
	serviceContext := context.New("BLINK-ALERT-DISPATCHER - DISPATCH")
	if err := configuration.LoadFromEnvironment(&serviceContext); err != nil {
		log.Fatalln(err)
	}
	serviceContext.Logger = logger.New(serviceContext.Name(), "dev")

	return &DispatcherService{
		ServiceContext:     serviceContext,
		dispatcherMessages: serviceContext.Messages().Subscribe(message.FormatService, true),
	}
}

// Name returns the dispatcher service name.
func (service *DispatcherService) Name() string { return "alert-dispatcher" }

// Run applies dispatchers to formatted alerts and publishes to the merger stage.
func (service *DispatcherService) Run() errors.Error {
	dispatcherRepo := dispatchers.GetDispatcherRepository()
	for {
		msg := <-service.dispatcherMessages
		alertMsg, ok := msg.(alerts.AlertMessage)
		if !ok {
			service.Error(errors.New("invalid message type"))
			continue
		}
		alert := alertMsg.Alert
		service.Info("applying dispatchers for alert %s", alert.AlertID)
		for _, name := range alert.Rule.Dispatchers() {
			disp, err := dispatcherRepo.GetDispatcher(name)
			if err != nil {
				service.Error(err)
				continue
			}
			sent, err := disp.Dispatch(alert)
			if err != nil {
				service.Error(err)
				continue
			}
			if !sent {
				log.Printf("dispatcher %s returned false for alert %s", disp.Name(), alert.AlertID)
			}
		}
		// Publish to merger stage for post-dispatch merging
		service.Messages().Publish(message.MergeService, alerts.AlertMessage{Alert: alert})
	}
}
