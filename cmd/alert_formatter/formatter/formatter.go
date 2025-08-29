package formatter

import (
	"log"

	"github.com/harishhary/blink/cmd/alert_formatter/internal/message"
	"github.com/harishhary/blink/internal/configuration"
	"github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/formatters"
)

// FormatterService formats enriched alerts and publishes to dispatcher.
type FormatterService struct {
	context.ServiceContext
	syncMessages      messaging.MessageQueue
	formatterMessages messaging.MessageQueue
}

// New constructs an alert formatter service.
func New() *FormatterService {
	serviceContext := context.New("BLINK-ALERT-FORMATTER - FORMAT")
	if err := configuration.LoadFromEnvironment(&serviceContext); err != nil {
		log.Fatalln(err)
	}
	serviceContext.Logger = logger.New(serviceContext.Name(), "dev")

	return &FormatterService{
		ServiceContext:    serviceContext,
		syncMessages:      serviceContext.Messages().Subscribe(message.SyncService, false),
		formatterMessages: serviceContext.Messages().Subscribe(message.FormatService, true),
	}
}

// Name returns the formatter service name.
func (service *FormatterService) Name() string { return "alert-formatter" }

// Run applies formatters to incoming alerts and publishes to dispatcher stage.
func (service *FormatterService) Run() errors.Error {
	formatterRepo := formatters.GetFormatterRepository()
	for {
		msg := <-service.formatterMessages
		alertMsg, ok := msg.(alerts.AlertMessage)
		if !ok {
			service.Error(errors.New("invalid message type"))
			continue
		}
		alert := alertMsg.Alert
		service.Info("applying formatters for alert %s", alert.AlertID)
		for _, name := range alert.Rule.Formatters() {
			fmttr, err := formatterRepo.Get(name)
			if err != nil {
				service.Error(err)
				continue
			}
			if !fmttr.Enabled() {
				service.Info("disabled formatter %s therefore skipping", fmttr.Name())
				continue
			}
			if _, err := fmttr.Format(alert); err != nil {
				service.Error(err)
			}
		}
		service.Messages().Publish(message.DispatchService, alerts.AlertMessage{Alert: alert})
	}
}
