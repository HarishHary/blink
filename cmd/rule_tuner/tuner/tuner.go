package tuner

import (
	"log"

	"github.com/harishhary/blink/cmd/rule_tuner/internal/message"
	"github.com/harishhary/blink/internal/configuration"
	"github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/rules/tuning_rules"
)

// TunerService applies tuning rules on incoming alerts and publishes to the enricher.
type TunerService struct {
	context.ServiceContext
	syncMessages  messaging.MessageQueue
	tunerMessages messaging.MessageQueue
}

// New constructs a rule tuner service.
func New() *TunerService {
	serviceContext := context.New("BLINK-RULE-TUNER - TUNER")
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

// Name returns the tuner service name.
func (service *TunerService) Name() string { return "rule-tuner" }

// Run processes tuning rules on alerts received from the executor stage.
func (service *TunerService) Run() errors.Error {
	tuningRepo := tuning_rules.GetTuningRuleRepository()
	go func() {
		recv := func() messaging.Message {
			msg := <-service.syncMessages
			service.Debug("received message: '%v'", msg)
			return msg
		}
		for {
			newMessage := recv()
			service.Debug("recording new message: '%v'", newMessage)
			tuningRepo.Record(newMessage)
		}
	}()

	for {
		msg := <-service.tunerMessages
		alertMsg, ok := msg.(alerts.AlertMessage)
		if !ok {
			service.Error(errors.New("invalid message type"))
			continue
		}
		alert := alertMsg.Alert
		service.Info("applying tuning rules for alert %s", alert.AlertID)
		var rulesList []tuning_rules.ITuningRule
		for _, name := range alert.Rule.TuningRules() {
			r, err := tuningRepo.Get(name)
			if err != nil {
				service.Error(err)
				continue
			}
			if !r.Enabled() {
				service.Info("disabled tuning rule %s therefore skipping", r.Name())
				continue
			}
			rulesList = append(rulesList, r)
		}
		confidence, err := tuning_rules.ProcessTuningRules(alert, rulesList)
		if err != nil {
			service.Error(err)
		}
		alert.Confidence = confidence
		service.Messages().Publish(message.EnricherService, alerts.AlertMessage{Alert: alert})
	}
}
