package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/runtime/controller"
	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/enrichments"
	"github.com/harishhary/blink/pkg/formatters"
	"github.com/harishhary/blink/pkg/matchers"
	"github.com/harishhary/blink/pkg/rules"
	"github.com/harishhary/blink/pkg/tuning_rules"
)

const runtimeShutdownTimeout = 45 * time.Second

type config struct {
	services.Common
	plugin.EtcdClusterConfig
	// ControllerNodeHost is this node's cluster-reachable name (its stable Service DNS name in
	// k8s). Empty keeps the "controller" default.
	ControllerNodeHost    string `env:"CONTROLLER_NODE_HOST,optional"`
	ControllerDatabaseDSN string `env:"CONTROLLER_DATABASE_DSN"`
	RulePluginDir         string `env:"RULE_PLUGIN_DIR"`
	MatcherPluginDir      string `env:"MATCHER_PLUGIN_DIR"`
	TuningPluginDir       string `env:"TUNER_PLUGIN_DIR"`
	FormatterPluginDir    string `env:"FORMATTER_PLUGIN_DIR"`
	EnrichmentPluginDir   string `env:"ENRICHER_PLUGIN_DIR"`
}

// controllerStatus returns a /status callback that queries every namespace's controller actor for
// its tracked executors, tolerating a namespace that has not bootstrapped yet.
func controllerStatus(node gen.Node, namespaces []string) func() any {
	return func() any {
		status := make(map[string]any, len(namespaces))
		for _, namespace := range namespaces {
			endpoint := gen.ProcessID{Name: gen.Atom("controller-" + namespace + "-actor"), Node: node.Name()}
			response, err := node.CallProcessID(endpoint, controller.StatusRequest{}, 1)
			if err != nil {
				status[namespace] = map[string]string{"error": err.Error()}
				continue
			}
			status[namespace] = response
		}
		return status
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var cfg config
	if err := services.LoadFromEnvironment(&cfg); err != nil {
		slog.Error("load controller config", "error", err)
		os.Exit(1)
	}
	rootLogger := logger.New("controller", cfg.Env)

	nodeHost := cfg.ControllerNodeHost
	if nodeHost == "" {
		nodeHost = "controller"
	}
	nodeName := gen.Atom("controller@" + nodeHost)
	registrar, err := plugin.NewEtcdRegistrar(cfg.EtcdClusterConfig, cfg.Env)
	if err != nil {
		rootLogger.FatalF("create etcd registrar: %v", err)
	}
	cluster := &plugin.ClusterOptions{Cookie: cfg.Cookie, Registrar: registrar, Flags: plugin.DefaultClusterFlags()}
	host, err := plugin.Start(plugin.NodeOptions{
		Name:            nodeName,
		Env:             cfg.Env,
		ShutdownTimeout: runtimeShutdownTimeout,
		Cluster:         cluster,
	})
	if err != nil {
		rootLogger.FatalF("start controller node: %v", err)
	}

	node := host.Node()
	runner := services.New(rootLogger.With("component", "runner"))
	ruleControllerSvc := controller.NewService(node, "controller-rule", controller.Options[*rules.RuleMetadata]{
		DatabaseDSN: cfg.ControllerDatabaseDSN,
		Namespace:   "rule",
		SupervisorOptions: controller.SupervisorOptions[*rules.RuleMetadata]{
			ActorOptions: controller.ActorOptions[*rules.RuleMetadata]{
				Directory: cfg.RulePluginDir,
				Loader:    rules.Loader{},
			},
		},
	})
	runner.Register(
		ruleControllerSvc,
		controller.NewService(node, "controller-matcher", controller.Options[*matchers.MatcherMetadata]{
			DatabaseDSN: cfg.ControllerDatabaseDSN,
			Namespace:   "matcher",
			SupervisorOptions: controller.SupervisorOptions[*matchers.MatcherMetadata]{
				ActorOptions: controller.ActorOptions[*matchers.MatcherMetadata]{
					Directory: cfg.MatcherPluginDir,
					Loader:    matchers.Loader{},
				},
			},
		}),
		controller.NewService(node, "controller-tuning", controller.Options[*tuning_rules.TuningRuleMetadata]{
			DatabaseDSN: cfg.ControllerDatabaseDSN,
			Namespace:   "tuning",
			SupervisorOptions: controller.SupervisorOptions[*tuning_rules.TuningRuleMetadata]{
				ActorOptions: controller.ActorOptions[*tuning_rules.TuningRuleMetadata]{
					Directory: cfg.TuningPluginDir,
					Loader:    tuning_rules.Loader{},
				},
			},
		}),
		controller.NewService(node, "controller-formatter", controller.Options[*formatters.FormatterMetadata]{
			DatabaseDSN: cfg.ControllerDatabaseDSN,
			Namespace:   "formatter",
			SupervisorOptions: controller.SupervisorOptions[*formatters.FormatterMetadata]{
				ActorOptions: controller.ActorOptions[*formatters.FormatterMetadata]{
					Directory: cfg.FormatterPluginDir,
					Loader:    formatters.Loader{},
				},
			},
		}),
		controller.NewService(node, "controller-enrichment", controller.Options[*enrichments.EnrichmentMetadata]{
			DatabaseDSN: cfg.ControllerDatabaseDSN,
			Namespace:   "enrichment",
			SupervisorOptions: controller.SupervisorOptions[*enrichments.EnrichmentMetadata]{
				ActorOptions: controller.ActorOptions[*enrichments.EnrichmentMetadata]{
					Directory: cfg.EnrichmentPluginDir,
					Loader:    enrichments.Loader{},
				},
			},
		}),
	)
	healthSvc := services.NewHealthService(":8080", nil, controllerStatus(node, []string{"rule", "matcher", "tuning", "formatter", "enrichment"}))
	runner.Register(healthSvc)
	runner.Run(ctx)

	closeCtx, cancel := context.WithTimeout(context.Background(), runtimeShutdownTimeout)
	err = host.Close(closeCtx)
	cancel()
	if err != nil {
		rootLogger.FatalF("close controller node: %v", err)
	}
	rootLogger.Info("Shutting down controller")
}
