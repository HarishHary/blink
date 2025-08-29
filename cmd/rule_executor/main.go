package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/harishhary/blink/cmd/rule_executor/executor"
	"github.com/harishhary/blink/cmd/rule_executor/sync"
	"github.com/harishhary/blink/internal/services"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	// HTTP server for metrics and health checks
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		http.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
		})
		http.HandleFunc("/health/ready", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
		})
		log.Fatal(http.ListenAndServe(":8080", nil))
	}()

	// Graceful shutdown on SIGTERM/SIGINT
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runner := services.New()
	runner.Register(
		sync.New(),
		executor.New(),
	)
	go runner.Run()

	<-ctx.Done()
	log.Println("Shutting down rule-executor")
}
