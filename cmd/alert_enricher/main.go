package main

import (
	"github.com/harishhary/blink/cmd/alert_enricher/enricher"
	"github.com/harishhary/blink/cmd/alert_enricher/sync"
	"github.com/harishhary/blink/internal/services"
)

func main() {
	runner := services.New()
	runner.Register(
		sync.New(),
		enricher.New(),
	)
	runner.Run()
}
