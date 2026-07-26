package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/harishhary/blink/cmd/alert_merger/merger"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/services"
)

// config contains the environment-loaded alert-merger settings.
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
	rootLogger := logger.New("alert-merger", cfg.Env)

	mergerSvc := merger.NewService(rootLogger.With("component", "service"), cfg.Config)

	go services.ServeHealth(rootLogger.With("component", "health"), ":8080", nil)

	runner := services.New(rootLogger.With("component", "runner"))
	runner.Register(mergerSvc)
	runner.Run(ctx)
	log.Println("Shutting down alert-merger")
}
