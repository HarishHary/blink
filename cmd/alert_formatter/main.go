package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/harishhary/blink/cmd/alert_formatter/formatter"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/formatters"
	formatterconfig "github.com/harishhary/blink/pkg/formatters/config"
	pools "github.com/harishhary/blink/internal/pools"
	fmtcatalog "github.com/harishhary/blink/pkg/formatters/pool"
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
	cfgWatcher, err := formatterconfig.NewWatcher(pluginDir)
	if err != nil {
		log.Fatalf("formatter config watcher: %v", err)
	}

	routingTable := pools.NewRoutingTable()
	formatterPool := fmtcatalog.NewPool(routingTable, 0)

	syncSvc, err := services.NewPluginSyncService(
		"alert-formatter-sync",
		"BLINK-ALERT-FORMATTER - SYNC",
		"FORMATTER_PLUGIN_DIR",
		func(log *logger.Logger, dir string) plugin.Plugin {
			return formatters.NewManager(log, formatterPool.Sync, dir, cfgWatcher)
		},
	)
	if err != nil {
		log.Fatalf("sync service: %v", err)
	}
	formatterSvc, err := formatter.NewFormatterService(formatterPool)
	if err != nil {
		log.Fatalf("formatter service: %v", err)
	}

	runner := services.New()
	runner.Register(
		cfgWatcher,
		syncSvc,
		formatterSvc,
	)
	runner.Run(ctx)
	log.Println("Shutting down alert-formatter")
}
