package main

import (
	"github.com/harishhary/blink/cmd/alert_dispatcher/dispatcher"
	"github.com/harishhary/blink/cmd/alert_dispatcher/sync"
	"github.com/harishhary/blink/internal/services"
)

func main() {
	runner := services.New()
	runner.Register(
		sync.New(),
		dispatcher.New(),
	)
	runner.Run()
}
