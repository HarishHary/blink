package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/harishhary/blink/cmd/rule_executor/executor"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/controller"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/rules"
)

// config is everything rule_executor needs. Every field is required (no ,optional) so a
// missing topic or dir fails fast at load instead of degrading silently at runtime.
// The embedded executor.Config carries the service's topics and its Tuning knobs; SnapshotTopic
// and PluginDir wire the control plane here in main. Broker is injected post-load.
type config struct {
	services.Common
	executor.Config
	SnapshotTopic string `env:"KAFKA_TOPIC_RULE_SNAPSHOT"`
	PluginDir     string `env:"RULE_PLUGIN_DIR"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var cfg config
	if err := services.LoadFromEnvironment(&cfg); err != nil {
		log.Fatalf("config: %v", err)
	}
	cfg.Config.Broker = brokers.NewKafkaBroker(cfg.Kafka)
	b := cfg.Config.Broker

	// snapSrc is the executor's reconcile feed (a SnapshotSource); source is the pool/adapter's
	// rollout view (a rules.Source). In normal mode these are two objects - a SnapshotReader
	// tailing Kafka and a SnapshotConfig viewing it. In LocalDev mode a single LocalReader is
	// BOTH: it reads YAML sidecars from PluginDir directly, serving Source[T] straight from disk
	// and electing a snapshot for the reconcile feed - no controller, no Kafka snapshot topic.
	var snapSrc plugin.SnapshotSource
	var source *rules.SnapshotConfig
	var ready func() bool
	var srcSvcs []*services.ManagedService

	reader := controller.NewSnapshotReader(logger.New("rule-snapshot", "dev"), b.NewBroadcastReader(cfg.SnapshotTopic))
	readerSvc := services.NewManagedService("rule-snapshot-sync", reader)
	snapSrc = reader
	source = rules.NewSnapshotConfig(logger.New("rule-config", "dev"), reader)
	ready = reader.Ready
	srcSvcs = []*services.ManagedService{readerSvc}

	rulePool := rules.NewPool(source, 0)

	pluginMgr := rules.NewPluginExecutor(logger.New("rule-executor", "dev"), rulePool.Sync, cfg.PluginDir, snapSrc, source)
	syncSvc := services.NewManagedService("rule-executor-sync", pluginMgr)

	executorSvc := executor.NewExecutorService(cfg.Config, rulePool, source)

	// Readiness: stay out of rotation until desired state has been delivered/elected.
	go services.ServeHealth(":8080", ready)

	runner := services.New()
	for _, s := range srcSvcs {
		runner.Register(s)
	}
	runner.Register(syncSvc, executorSvc)
	runner.Run(ctx)
	log.Println("Shutting down rule-executor")
}
