package main

import (
	"github.com/harishhary/blink/cmd/alert_engine/enrich"
	"github.com/harishhary/blink/cmd/alert_engine/sync"
	"github.com/harishhary/blink/cmd/alert_engine/tune"
	"github.com/harishhary/blink/internal/services"
)

func main() {
	runner := services.New()
	runner.Register(
		sync.New(),
		tune.New(),
		enrich.New(),
	// events.New(config),
	// http.New(config),
	// grpc.New(config),
	)

	runner.Run()
}
