package tune

import (
	"log"

	"github.com/harishhary/blink/cmd/alert_engine/internal/message"
	"github.com/harishhary/blink/internal/configuration"
	"github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/rules/tuning_rules"
)

type TunerService struct {
	context.ServiceContext
	syncMessages  messaging.MessageQueue
	tunerMessages messaging.MessageQueue
}

func New() *TunerService {
	serviceContext := context.New("BLINK-ALERT-ENGINE - TUNER")
	if err := configuration.LoadFromEnvironment(&serviceContext); err != nil {
		log.Fatalln(err)
	}
	serviceContext.Logger = logger.New(serviceContext.Name(), "dev")

	return &TunerService{
		ServiceContext: serviceContext,
		syncMessages:   serviceContext.Messages().Subscribe(message.SyncService, false),
		tunerMessages:  serviceContext.Messages().Subscribe(message.TunerService, true),
	}
}

func (service *TunerService) Run() errors.Error {
	service.Info("getting tuning rules...")
	tuningRules := tuning_rules.GetTuningRuleRepository()

	go func() {
		recv := func() messaging.Message {
			msg := <-service.syncMessages
			service.Debug("received message: '%v'", msg)
			return msg
		}

		for {
			newMessage := recv()
			service.Debug("recording new message: '%v'", newMessage)
			tuningRules.Record(newMessage)
		}
	}()

	for {
		newMessage := <-service.tunerMessages
		newAlert, ok := newMessage.(alerts.AlertMessage)
		if !ok {
			service.ErrorF("invalid message type")
			continue
		}

		alert := newAlert.Alert
		service.Info("applying tuning rules for alert %s", alert.AlertID)
		if err := applyTuningRules(service, &alert); err != nil {
			service.Error(err)
		}

		service.Info("sending alert %s to alert queue", alert.AlertID)
		service.Messages().Publish(message.AlertService, alerts.AlertMessage{
			Alert: alert,
		})
	}
}

func applyTuningRules(service *TunerService, alert *alerts.Alert) errors.Error {
	tuningRepository := tuning_rules.GetTuningRuleRepository()
	tuningRules := []tuning_rules.ITuningRule{}

	for _, rule := range alert.Rule.TuningRules() {
		rule, err := tuningRepository.Get(rule)
		if err != nil {
			service.Error(err)
			continue
		}
		if !rule.Enabled() {
			service.Info("disabled tuning rule %s therefore skipping", rule.Name())
			continue
		}
		tuningRules = append(tuningRules, rule)
	}
	confidence, err := tuning_rules.ProcessTuningRules(*alert, tuningRules)
	if err != nil {
		service.Error(err)
		return err
	}
	alert.Confidence = confidence
	return nil
}
