package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/harishhary/blink/cmd/alert_formatter/formatter"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/configuration"
	"github.com/harishhary/blink/internal/controller"
	"github.com/harishhary/blink/internal/logger"
	pools "github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/formatters"
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

	pluginDir := os.Getenv("FORMATTER_PLUGIN_DIR")
	cfgMgr := formatters.NewFormatterConfigWatcher(logger.New("formatter-config", "dev"), pluginDir)
	cfgSvc := services.NewConfigSyncService("formatter-config-sync", "BLINK-ALERT-FORMATTER - CONFIG", cfgMgr)

	// Read replica: consumes the formatter controller's snapshot topic and feeds the
	// executor the control plane's desired state.
	var cfg configuration.ServiceConfiguration
	if err := configuration.LoadFromEnvironment(&cfg); err != nil {
		log.Fatalf("config: %v", err)
	}
	b := brokers.NewKafkaBroker(cfg.Kafka)
	replica := controller.NewReplica(
		logger.New("formatter-snapshot", "dev"),
		b.NewReader(cfg.Topics.FormatterSnapshotTopic, cfg.Topics.FormatterSnapshotGroup),
	)
	replicaSvc, err := services.NewPluginSyncService("formatter-snapshot-sync", "BLINK-ALERT-FORMATTER - SNAPSHOT", replica)
	if err != nil {
		log.Fatalf("snapshot service: %v", err)
	}

	routingTable := pools.NewRoutingTable()
	formatterPool := formatters.NewPool(routingTable, 0)

	pluginMgr := formatters.NewFormatterPluginExecutor(logger.New("alert-formatter", "dev"), formatterPool.Sync, pluginDir, replica, cfgMgr)
	syncSvc, err := services.NewPluginSyncService("alert-formatter-sync", "BLINK-ALERT-FORMATTER - SYNC", pluginMgr)
	if err != nil {
		log.Fatalf("sync service: %v", err)
	}
	formatterSvc, err := formatter.NewFormatterService(formatterPool)
	if err != nil {
		log.Fatalf("formatter service: %v", err)
	}

	runner := services.New()
	runner.Register(
		cfgSvc,
		replicaSvc,
		syncSvc,
		formatterSvc,
	)
	runner.Run(ctx)
	log.Println("Shutting down alert-formatter")
}
