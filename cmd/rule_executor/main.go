package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/harishhary/blink/cmd/rule_executor/executor"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/configuration"
	"github.com/harishhary/blink/internal/controller"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/rules"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		http.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		http.HandleFunc("/health/ready", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		log.Fatal(http.ListenAndServe(":8080", nil))
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// RULE_PLUGIN_DIR contains both the rule binaries and their .yaml sidecars.
	// The config manager must start before the rule manager so YAML configs are
	// available when binaries are first discovered.
	rulePluginDir := os.Getenv("RULE_PLUGIN_DIR")
	if rulePluginDir == "" {
		log.Fatal("RULE_PLUGIN_DIR is required")
	}
	cfgMgr := rules.NewRuleConfigWatcher(logger.New("rule-config", "dev"), rulePluginDir)
	cfgSvc := services.NewConfigSyncService("rule-config-sync", "BLINK-RULE-EXECUTOR - CONFIG", cfgMgr)

	// Read replica: consumes the rule controller's snapshot topic and feeds the
	// executor the control plane's desired state.
	var cfg configuration.ServiceConfiguration
	if err := configuration.LoadFromEnvironment(&cfg); err != nil {
		log.Fatalf("config: %v", err)
	}
	b := brokers.NewKafkaBroker(cfg.Kafka)
	replica := controller.NewReplica(
		logger.New("rule-snapshot", "dev"),
		b.NewReader(cfg.Topics.RuleSnapshotTopic, cfg.Topics.RuleSnapshotGroup),
	)
	replicaSvc, err := services.NewPluginSyncService("rule-snapshot-sync", "BLINK-RULE-EXECUTOR - SNAPSHOT", replica)
	if err != nil {
		log.Fatalf("snapshot service: %v", err)
	}

	rulePool := rules.NewPool(cfgMgr, 0)

	pluginMgr := rules.NewRulePluginExecutor(logger.New("rule-executor", "dev"), rulePool.Sync, rulePluginDir, replica, cfgMgr)
	syncSvc, err := services.NewPluginSyncService("rule-executor-sync", "BLINK-RULE-EXECUTOR - SYNC", pluginMgr)
	if err != nil {
		log.Fatalf("sync service: %v", err)
	}

	executorSvc, err := executor.NewExecutorService(rulePool, cfgMgr)
	if err != nil {
		log.Fatalf("executor service: %v", err)
	}

	runner := services.New()
	runner.Register(
		cfgSvc,
		replicaSvc,
		syncSvc,
		executorSvc,
	)
	runner.Run(ctx)
	log.Println("Shutting down rule-executor")
}
