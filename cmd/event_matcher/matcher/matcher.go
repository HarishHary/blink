package matcher

import (
	stdctx "context"
	"encoding/json"
	"log"

	bkr "github.com/harishhary/blink/internal/broker"
	"github.com/harishhary/blink/internal/broker/kafka"
	"github.com/harishhary/blink/internal/configuration"
	ctx "github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/pkg/rules"
)

// MatcherService processes incoming events from Kafka and publishes matched ones.
type MatcherService struct {
	ctx.ServiceContext
	reader   bkr.Reader
	writer   bkr.Writer
	ruleRepo *rules.RuleRepository
}

// New constructs an event matcher service using Kafka.
func New() *MatcherService {
	serviceContext := ctx.New("BLINK-EVENT-MATCHER - MATCHER")
	if err := configuration.LoadFromEnvironment(&serviceContext); err != nil {
		log.Fatalln(err)
	}
	serviceContext.Logger = logger.New(serviceContext.Name(), "dev")

	broker := kafka.NewKafkaBroker(serviceContext.Configuration().Kafka)
	readr := broker.NewReader(
		serviceContext.Configuration().Topics.MatcherTopic,
		serviceContext.Configuration().Topics.MatcherGroup,
	)
	writer := broker.NewWriter(serviceContext.Configuration().Topics.ExecTopic)

	return &MatcherService{
		ServiceContext: serviceContext,
		reader:         readr,
		writer:         writer,
		ruleRepo:       rules.GetRuleRepository(),
	}
}

// Name returns the matcher service name.
func (service *MatcherService) Name() string { return "event-matcher" }

// Run reads raw events, applies matchers, and writes matched events.
func (service *MatcherService) Run() errors.Error {
	ctx := stdctx.Background()
	for {
		msg, err := service.reader.ReadMessage(ctx)
		if err != nil {
			service.Error(errors.NewE(err))
			continue
		}
		var evt map[string]any
		if err := json.Unmarshal(msg.Value, &evt); err != nil {
			service.Error(errors.NewE(err))
			continue
		}
		if logType, ok := evt["log_type"].(string); ok {
			service.Info("matching event for log_type %s", logType)
			for _, rule := range service.ruleRepo.GetRulesForLogType(logType) {
				if rule.Enabled() && rule.ApplyMatchers(evt) {
					payload, _ := json.Marshal(evt)
					if err := service.writer.WriteMessages(ctx,
						bkr.Message{Key: msg.Key, Value: payload}); err != nil {
						service.Error(errors.NewE(err))
					}
				}
			}
		}
	}
}
