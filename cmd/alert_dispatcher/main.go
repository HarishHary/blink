package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/harishhary/blink/cmd/alert_dispatcher/dispatcher"
	"github.com/harishhary/blink/cmd/alert_dispatcher/sync"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/dispatchers"
	"github.com/harishhary/blink/internal/services"
)

// config is everything alert_dispatcher needs. Required fields fail fast at load.
type config struct {
	services.Common
	DispatcherTopic string `env:"KAFKA_TOPIC_DISPATCHER"`
	DispatcherGroup string `env:"KAFKA_GROUP_DISPATCHER"`
	ConfigDir       string `env:"DISPATCHER_CONFIG_DIR"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var cfg config
	if err := services.LoadFromEnvironment(&cfg); err != nil {
		log.Fatalf("config: %v", err)
	}
	b := brokers.NewKafkaBroker(cfg.Kafka)

	dispatcherRepo := dispatchers.NewDispatcherRepository()

	syncSvc := sync.New(sync.Config{ConfigDir: cfg.ConfigDir}, &dispatcherRepo)
	dispatcherSvc := dispatcher.New(dispatcher.Config{
		Broker:          b,
		DispatcherTopic: cfg.DispatcherTopic,
		DispatcherGroup: cfg.DispatcherGroup,
	}, &dispatcherRepo)

	go services.ServeHealth(":8080", nil) // no snapshot dependency - always ready once up

	runner := services.New()
	runner.Register(
		syncSvc,
		dispatcherSvc,
	)
	runner.Run(ctx)
	log.Println("Shutting down alert-dispatcher")
}
