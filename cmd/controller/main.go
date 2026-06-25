package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/harishhary/blink/internal/backends"
	"github.com/harishhary/blink/internal/brokers"
	cfg "github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/configuration"
	"github.com/harishhary/blink/internal/controller"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/enrichments"
	"github.com/harishhary/blink/pkg/formatters"
	"github.com/harishhary/blink/pkg/matchers"
	"github.com/harishhary/blink/pkg/rules"
	"github.com/harishhary/blink/pkg/tuning_rules"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// controller is the unified control plane: one PluginController per plugin type, each
// reconciling its own plugin dir + YAML and publishing its effective Snapshot to its own
// topic. Pipeline services consume those topics via their read replicas. Scale horizontally
// by running N replicas of this binary.
func main() {
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		http.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		http.HandleFunc("/health/ready", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		log.Fatal(http.ListenAndServe(":8080", nil))
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var svcCfg configuration.ServiceConfiguration
	if err := configuration.LoadFromEnvironment(&svcCfg); err != nil {
		log.Fatalf("config: %v", err)
	}
	b := brokers.NewKafkaBroker(svcCfg.Kafka)

	// Control-plane persistence shared by every type. NewNop is the no-op default;
	// swap for a real store (SQLite/Postgres) when persistence is needed.
	db := backends.NewNop()

	runner := services.New()

	ruleDir := mustGetenv("RULE_PLUGIN_DIR")
	addController(runner, "rule", db,
		rules.NewRuleConfigWatcher(logger.New("rule-config", "dev"), ruleDir),
		ruleDir, b.NewWriter(svcCfg.Topics.RuleSnapshotTopic))

	matcherDir := mustGetenv("MATCHER_PLUGIN_DIR")
	addController(runner, "matcher", db,
		matchers.NewMatcherConfigWatcher(logger.New("matcher-config", "dev"), matcherDir),
		matcherDir, b.NewWriter(svcCfg.Topics.MatcherSnapshotTopic))

	tuningDir := mustGetenv("TUNER_PLUGIN_DIR")
	addController(runner, "tuning", db,
		tuning_rules.NewTuningRuleConfigWatcher(logger.New("tuning-config", "dev"), tuningDir),
		tuningDir, b.NewWriter(svcCfg.Topics.TuningSnapshotTopic))

	formatterDir := mustGetenv("FORMATTER_PLUGIN_DIR")
	addController(runner, "formatter", db,
		formatters.NewFormatterConfigWatcher(logger.New("formatter-config", "dev"), formatterDir),
		formatterDir, b.NewWriter(svcCfg.Topics.FormatterSnapshotTopic))

	enrichmentDir := mustGetenv("ENRICHER_PLUGIN_DIR")
	addController(runner, "enrichment", db,
		enrichments.NewEnrichmentConfigWatcher(logger.New("enrichment-config", "dev"), enrichmentDir),
		enrichmentDir, b.NewWriter(svcCfg.Topics.EnrichmentSnapshotTopic))

	runner.Run(ctx)
	log.Println("Shutting down controller")
}

// addController wires a config watcher + PluginController for one plugin type and registers
// both as services: the watcher keeps Current() fresh; the controller reconciles and publishes.
func addController[T plugin.Syncable](runner *services.Runner, name string, db backends.Database, cfgMgr *cfg.ConfigWatcher[T], dir string, writer brokers.Writer) {
	cfgSvc := services.NewConfigSyncService(name+"-config-sync", "BLINK-CONTROLLER - "+name+" CONFIG", cfgMgr)
	ctrl := controller.NewPluginController[T](logger.New(name+"-controller", "dev"), db, cfgMgr, dir, writer)
	ctrlSvc := services.NewConfigSyncService(name+"-controller-sync", "BLINK-CONTROLLER - "+name+" CTRL", ctrl)
	runner.Register(cfgSvc, ctrlSvc)
}

func mustGetenv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("%s is required", key)
	}
	return v
}
