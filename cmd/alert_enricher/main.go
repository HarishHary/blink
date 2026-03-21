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
	"github.com/harishhary/blink/internal/pluginmgr"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/enrichments"
	pools "github.com/harishhary/blink/internal/pools"
	enrichcatalog "github.com/harishhary/blink/pkg/enrichments/pool"
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

	routingTable := pools.NewRoutingTable()
	enricherPool := enrichcatalog.NewPool(routingTable, 0)

	syncSvc, err := services.NewPluginSyncService(
		"alert-enricher-sync",
		"BLINK-ALERT-ENRICHER - SYNC",
		"ENRICHER_PLUGIN_DIR",
		func(log *logger.Logger, dir string) pluginmgr.Plugin {
			return enrichments.NewManager(log, enricherPool.Sync, dir)
		},
	)
	if err != nil {
		log.Fatalf("sync service: %v", err)
	}
	enricherSvc, err := enricher.NewEnricherService(enricherPool)
	if err != nil {
		log.Fatalf("enricher service: %v", err)
	}

	runner := services.New()
	runner.Register(
		syncSvc,
		enricherSvc,
	)
	runner.Run(ctx)
	log.Println("Shutting down alert-enricher")
}
