package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/harishhary/blink/cmd/alert_enricher/enricher"
	"github.com/harishhary/blink/internal/logger"
	pools "github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/enrichments"
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

	pluginDir := os.Getenv("ENRICHER_PLUGIN_DIR")
	cfgMgr := enrichments.NewEnrichmentConfigManager(logger.New("enrichment-config", "dev"), pluginDir)
	cfgSvc := services.NewConfigSyncService("enrichment-config-sync", "BLINK-ALERT-ENRICHER - CONFIG", cfgMgr)

	routingTable := pools.NewRoutingTable()
	enricherPool := enrichments.NewPool(routingTable, 0)

	pluginMgr := enrichments.NewEnrichmentPluginExecutor(logger.New("enricher", "dev"), enricherPool.Sync, pluginDir, cfgMgr)
	syncSvc, err := services.NewPluginSyncService("alert-enricher-sync", "BLINK-ALERT-ENRICHER - SYNC", pluginMgr)
	if err != nil {
		log.Fatalf("sync service: %v", err)
	}

	enricherSvc, err := enricher.NewEnricherService(enricherPool)
	if err != nil {
		log.Fatalf("enricher service: %v", err)
	}

	runner := services.New()
	runner.Register(
		cfgSvc,
		syncSvc,
		enricherSvc,
	)
	runner.Run(ctx)
	log.Println("Shutting down alert-enricher")
}
