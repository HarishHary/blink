package main

import (
	"github.com/harishhary/blink/cmd/rule_engine/exec"
	"github.com/harishhary/blink/cmd/rule_engine/sync"
	"github.com/harishhary/blink/internal/services"
)

func main() {
	runner := services.New()
	runner.Register(
		exec.New(),
		sync.New(),
		// events.New(config),
		// http.New(config),
		// grpc.New(config),
	)

	runner.Run()
}
