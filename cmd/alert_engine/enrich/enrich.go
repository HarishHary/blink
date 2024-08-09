package enrich

import (
	"log"
	"time"

	ctx "context"

	"github.com/harishhary/blink/cmd/alert_engine/internal/message"
	"github.com/harishhary/blink/internal/configuration"
	"github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/enrichments"
)

type EnricherService struct {
	context.ServiceContext
	syncMessages     messaging.MessageQueue
	enricherMessages messaging.MessageQueue
}

func New() *EnricherService {
	serviceContext := context.New("BLINK-ALERT-ENGINE - TUNER")
	if err := configuration.LoadFromEnvironment(&serviceContext); err != nil {
		log.Fatalln(err)
	}
	serviceContext.Logger = logger.New(serviceContext.Name(), "dev")

	return &EnricherService{
		ServiceContext:   serviceContext,
		syncMessages:     serviceContext.Messages().Subscribe(message.SyncService, false),
		enricherMessages: serviceContext.Messages().Subscribe(message.EnricherService, true),
	}
}

func (service *EnricherService) Run() errors.Error {
	service.Info("getting enrichment...")
	enrichmentRepository := enrichments.GetEnrichmentRepository()

	go func() {
		recv := func() messaging.Message {
			msg := <-service.syncMessages
			service.Debug("received message: '%v'", msg)
			return msg
		}

		for {
			newMessage := recv()
			service.Debug("recording new message: '%v'", newMessage)
			enrichmentRepository.Record(newMessage)
		}
	}()

	for {
		newMessage := <-service.enricherMessages
		newAlert, ok := newMessage.(alerts.AlertMessage)
		if !ok {
			service.ErrorF("invalid message type")
			continue
		}

		alert := newAlert.Alert
		service.Info("applying enrichments for alert %s", alert.AlertID)
		if err := applyEnrichments(service, &alert); err != nil {
			service.Error(err)
		}

		service.Info("sending alert %s to tuner queue", alert.AlertID)
		service.Messages().Publish(message.TunerService, alerts.AlertMessage{
			Alert: alert,
		})
	}
}

func applyEnrichments(service *EnricherService, alert *alerts.Alert) errors.Error {
	enrichmentRepository := enrichments.GetEnrichmentRepository()
	for _, enrichment := range alert.Rule.Enrichments() {
		enrichment, err := enrichmentRepository.Get(enrichment)
		if err != nil {
			service.Error(err)
			continue
		}
		if !enrichment.Enabled() {
			service.Info("disabled enrichment %s therefore skipping", enrichment.Name())
			continue
		}
		service.Info("applying enrichment %s for alert %s", enrichment.Name(), alert.AlertID)
		// Create a context with timeout
		context, cancel := ctx.WithTimeout(ctx.Background(), 30*time.Second)
		defer cancel()

		done := make(chan errors.Error, 1)
		go func() {
			done <- enrichment.Enrich(context, alert)
		}()

		select {
		case err := <-done:
			if err != nil {
				service.Error(err)
			}
		case <-context.Done():
			if context.Err() == ctx.DeadlineExceeded {
				service.Error(errors.NewF("enrichment %s timed out", enrichment.Name()))
			} else {
				service.Error(errors.NewE(context.Err()))
			}
		}
	}
	return nil
}
