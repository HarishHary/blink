package main

import (
	"github.com/harishhary/blink/cmd/alert_merger/merger"
	"github.com/harishhary/blink/cmd/alert_merger/sync"
	"github.com/harishhary/blink/internal/services"
)

func main() {
	runner := services.New()
	runner.Register(
		sync.New(),
		merger.New(),
	)
	runner.Run()
}
