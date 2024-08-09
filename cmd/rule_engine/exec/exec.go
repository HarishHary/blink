package exec

import (
	"log"
	"time"

	ctx "context"

	"github.com/harishhary/blink/cmd/rule_engine/internal/message"
	"github.com/harishhary/blink/internal/configuration"
	"github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/enrichments"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/rules"
	"github.com/harishhary/blink/pkg/rules/tuning_rules"
)

type ExecService struct {
	context.ServiceContext
	syncMessages  messaging.MessageQueue
	eventMessages messaging.MessageQueue
}

func New() *ExecService {
	serviceContext := context.New("BLINK-NODE - EXEC")
	if err := configuration.LoadFromEnvironment(&serviceContext); err != nil {
		log.Fatalln(err)
	}
	serviceContext.Logger = logger.New(serviceContext.Name(), "dev")

	return &ExecService{
		ServiceContext: serviceContext,
		syncMessages:   serviceContext.Messages().Subscribe(message.SyncService, false),
		eventMessages:  serviceContext.Messages().Subscribe(message.EventService, true),
	}
}

func (service *ExecService) Run() errors.Error {
	ruleRepository := rules.GetRuleRepository()
	for {
		newmessage := <-service.eventMessages
		newevent, ok := newmessage.(events.EventMessage)
		if !ok {
			service.ErrorF("invalid message type")
			continue
		}

		event := newevent.Event
		logType, ok := event["log_type"].(string)
		if !ok {
			service.ErrorF("missing log_type in event")
			continue
		}

		service.Info("evaluating rules for log_type %s", logType)
		rules := ruleRepository.GetRulesForLogType(logType)
		for _, rule := range rules {
			if rule.Enabled() {
				if !rule.SubKeysInEvent(event) {
					continue
				}

				service.Info("applying matchers for rule %s", rule.Name())
				if !rule.ApplyMatchers(event) {
					continue
				}

				service.Info("evaluating rule %s", rule.Name())
				ruleResult, err := rule.Evaluate(event)
				if err != nil {
					service.Error(err)
					continue
				}
				if ruleResult {
					service.Info("rule %s passed", rule.Name())
					alert, err := alerts.NewAlert(
						rule,
						event,
					)
					if err != nil {
						service.Error(err)
						continue
					}

					service.Info("applying enrichments for alert %s", alert.AlertID)
					if err := applyEnrichments(service, alert); err != nil {
						service.Error(err)
					}

					service.Info("applying tuning rules for alert %s", alert.AlertID)
					if err := applyTuningRules(service, alert); err != nil {
						service.Error(err)
					}

					service.Info("creating alert %s", alert.AlertID)
					service.Messages().Publish(message.AlertService, alerts.AlertMessage{
						Alert: *alert,
					})
				}
			}
		}
	}
}

func applyTuningRules(service *ExecService, alert *alerts.Alert) errors.Error {
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

func applyEnrichments(service *ExecService, alert *alerts.Alert) errors.Error {
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
