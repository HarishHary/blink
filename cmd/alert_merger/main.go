package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/harishhary/blink/cmd/alert_merger/merger"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/services"
)

// config is everything alert_merger needs. Required fields fail fast at load.
// The embedded merger.Config carries the topic/group fields; Broker is injected post-load.
type config struct {
	services.Common
	merger.Config
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var cfg config
	if err := services.LoadFromEnvironment(&cfg); err != nil {
		log.Fatalf("config: %v", err)
	}
	cfg.Config.Broker = brokers.NewKafkaBroker(cfg.Kafka)

	mergerSvc := merger.NewService(cfg.Config)

	go services.ServeHealth(":8080", nil)

	runner := services.New()
	runner.Register(mergerSvc)
	runner.Run(ctx)
	log.Println("Shutting down alert-merger")
}
