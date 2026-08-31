package main

import (
	"context"
	"os"
	"os/signal"
	"slog"
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
			endpoint := gen.ProcessID{Name: controller.ActorName(namespace), Node: node.Name()}
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

// main starts the controller node, one controller service per namespace, and the health endpoint, and serves them until the process is signalled.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var cfg config
	if err := services.LoadFromEnvironment(&cfg); err != nil {
		slog.Fatalf("load controller config: %v", err)
	}
	rootLogger := logger.New("controller", cfg.Debug)

	nodeHost := cfg.ControllerNodeHost
	if nodeHost == "" {
		nodeHost = "controller"
	}
	nodeName := gen.Atom("controller@" + nodeHost)
	registrar, err := plugin.NewEtcdRegistrar(cfg.EtcdClusterConfig, cfg.Env)
	if err != nil {
		rootLogger.FatalF("create etcd registrar: %v", err)
	}
	cluster := &plugin.ClusterOptions{Cookie: cfg.Cookie, Port: cfg.Port, Registrar: registrar, Flags: plugin.DefaultClusterFlags()}
	host, err := plugin.Start(plugin.NodeOptions{
		Name:            nodeName,
		Debug:           cfg.Debug,
		ShutdownTimeout: runtimeShutdownTimeout,
		Cluster:         cluster,
		Observer:        plugin.EndpointOptions{Enabled: cfg.ObserverEnabled, Host: cfg.ObserverHost, Port: cfg.ObserverPort},
		MCP:             plugin.EndpointOptions{Enabled: cfg.MCPEnabled, Host: cfg.MCPHost, Port: cfg.MCPPort},
		Radar:           plugin.EndpointOptions{Enabled: cfg.RadarEnabled, Host: cfg.RadarHost, Port: cfg.RadarPort},
	})
	if err != nil {
		rootLogger.FatalF("start controller node: %v", err)
	}

	node := host.Node()
	runner := services.New(rootLogger.With("component", "runner"))
	ruleControllerSvc := controller.NewService(node, "controller-rule", controller.ApplicationOptions{
		DatabaseDSN: cfg.ControllerDatabaseDSN,
		Namespace:   "rule",
		SupervisorOptions: controller.SupervisorOptions{
			ActorOptions: controller.ActorOptions{
				Directory: cfg.RulePluginDir,
			},
		},
	}, rules.Loader{})

	matcherControllerSvc := controller.NewService(node, "controller-matcher", controller.ApplicationOptions{
		DatabaseDSN: cfg.ControllerDatabaseDSN,
		Namespace:   "matcher",
		SupervisorOptions: controller.SupervisorOptions{
			ActorOptions: controller.ActorOptions{
				Directory: cfg.MatcherPluginDir,
			},
		},
	}, matchers.Loader{})

	tuningRulesControllerSvc := controller.NewService(node, "controller-tuning", controller.ApplicationOptions{
		DatabaseDSN: cfg.ControllerDatabaseDSN,
		Namespace:   "tuning",
		SupervisorOptions: controller.SupervisorOptions{
			ActorOptions: controller.ActorOptions{
				Directory: cfg.TuningPluginDir,
			},
		},
	}, tuning_rules.Loader{})

	formattersControllerSvc := controller.NewService(node, "controller-formatter", controller.ApplicationOptions{
		DatabaseDSN: cfg.ControllerDatabaseDSN,
		Namespace:   "formatter",
		SupervisorOptions: controller.SupervisorOptions{
			ActorOptions: controller.ActorOptions{
				Directory: cfg.FormatterPluginDir,
			},
		},
	}, formatters.Loader{})

	enrichmentControllerSvc := controller.NewService(node, "controller-enrichment", controller.ApplicationOptions{
		DatabaseDSN: cfg.ControllerDatabaseDSN,
		Namespace:   "enrichment",
		SupervisorOptions: controller.SupervisorOptions{
			ActorOptions: controller.ActorOptions{
				Directory: cfg.EnrichmentPluginDir,
			},
		},
	}, enrichments.Loader{})

	healthSvc := services.NewHealthService(":8080", nil, controllerStatus(node, []string{"rule", "matcher", "tuning", "formatter", "enrichment"}))
	runner.Register(
		ruleControllerSvc,
		matcherControllerSvc,
		tuningRulesControllerSvc,
		formattersControllerSvc,
		enrichmentControllerSvc,
		healthSvc,
	)
	runner.Run(ctx)
	closeCtx, cancel := context.WithTimeout(context.Background(), runtimeShutdownTimeout)
	defer cancel()
	if err := host.Close(closeCtx); err != nil {
		rootLogger.FatalF("close controller node: %v", err)
	}
	rootLogger.Info("Controller shut down.")
}
