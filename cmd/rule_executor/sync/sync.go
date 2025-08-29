package sync

import (
	"log"
	"os"
	"time"

	"github.com/harishhary/blink/cmd/rule_executor/internal/message"
	"github.com/harishhary/blink/internal/configuration"
	"github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/repository"
	"github.com/harishhary/blink/pkg/rules"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	syncPluginLoadDuration = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor_sync", Name: "initial_plugin_load_seconds"})
	syncPluginLoadErrors   = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor_sync", Name: "initial_plugin_load_errors_total"})

	syncCycleDuration = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor_sync", Name: "sync_cycle_seconds"})
	syncCycleErrors   = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor_sync", Name: "sync_cycle_errors_total"})
	syncRulesAdded    = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor_sync", Name: "rules_added_total"})
	syncRulesDeleted  = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor_sync", Name: "rules_deleted_total"})

	syncMessagesReceived = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor_sync", Name: "sync_messages_received_total"})
)

// SyncService hot-loads rule plugins and broadcasts register/unregister messages.
type SyncService struct {
	context.ServiceContext
	syncMessages messaging.MessageQueue
}

// New constructs the rule-executor sync service.
func New() *SyncService {
	serviceContext := context.New("BLINK-RULE-EXECUTOR - SYNC")
	if err := configuration.LoadFromEnvironment(&serviceContext); err != nil {
		log.Fatalln(err)
	}
	serviceContext.Logger = logger.New(serviceContext.Name(), "dev")

	return &SyncService{
		ServiceContext: serviceContext,
		syncMessages:   serviceContext.Messages().Subscribe(message.SyncService, false),
	}
}

// Name returns the sync service name.
func (service *SyncService) Name() string { return "rule-executor-sync" }

// Run begins hot-loading rule plugins and syncing with the plugin directory.
func (service *SyncService) Run() errors.Error {
	ruleRepo := rules.GetRuleRepository()
	ruleDir := os.Getenv("RULE_PLUGIN_DIR")
	// initial plugin loading
	startInit := time.Now()
	if err := ruleRepo.Load(ruleDir); err != nil {
		syncPluginLoadErrors.Inc()
		service.Error(err)
	}
	syncPluginLoadDuration.Observe(time.Since(startInit).Seconds())

	// hot-update subscription: record incoming sync messages
	go func() {
		for msg := range service.syncMessages {
			syncMessagesReceived.Inc()
			service.Debug("received sync message: '%v'", msg)
			ruleRepo.Record(msg)
		}
	}()

	for {
		startCycle := time.Now()
		service.Info("syncing rule plugins...")
		time.Sleep(10 * time.Second)

		tempRepo := repository.NewRepository[rules.IRule]()
		if err := tempRepo.Load(ruleDir); err != nil {
			syncCycleErrors.Inc()
			service.Error(err)
			syncCycleDuration.Observe(time.Since(startCycle).Seconds())
			continue
		}
		toAdd, toDelete := ruleRepo.Diff(tempRepo)
		if len(toAdd) == 0 && len(toDelete) == 0 {
			syncCycleDuration.Observe(time.Since(startCycle).Seconds())
			continue
		}
		syncRulesAdded.Add(float64(len(toAdd)))
		syncRulesDeleted.Add(float64(len(toDelete)))
		service.Info("%d rule(s) to add", len(toAdd))
		service.Info("%d rule(s) to delete", len(toDelete))
		for _, entry := range toAdd {
			service.Debug("publishing register message for '%s'", entry.Name())
			service.Messages().Publish(message.SyncService, repository.NewRegisterMessage[rules.IRule](entry))
		}
		for _, instanceID := range toDelete {
			service.Debug("publishing unregister message for '%s'", instanceID)
			service.Messages().Publish(message.SyncService, repository.NewUnregisterMessage[rules.IRule](instanceID))
		}
		syncCycleDuration.Observe(time.Since(startCycle).Seconds())
	}
}
