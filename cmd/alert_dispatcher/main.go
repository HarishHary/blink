package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/harishhary/blink/cmd/alert_dispatcher/dispatcher"
	"github.com/harishhary/blink/cmd/alert_dispatcher/sync"
	"github.com/harishhary/blink/internal/dispatchers"
	"github.com/harishhary/blink/internal/services"
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

	dispatcherRepo := dispatchers.NewDispatcherRepository()

	syncSvc, err := sync.New(&dispatcherRepo)
	if err != nil {
		log.Fatal(err)
	}
	dispatcherSvc, err := dispatcher.New(&dispatcherRepo)
	if err != nil {
		log.Fatal(err)
	}

	runner := services.New()
	runner.Register(
		syncSvc,
		dispatcherSvc,
	)
	runner.Run(ctx)
	log.Println("Shutting down alert-dispatcher")
}
