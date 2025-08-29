package main

import (
	"github.com/harishhary/blink/cmd/rule_tuner/sync"
	"github.com/harishhary/blink/cmd/rule_tuner/tuner"
	"github.com/harishhary/blink/internal/services"
)

func main() {
	runner := services.New()
	runner.Register(
		sync.New(),
		tuner.New(),
	)
	runner.Run()
}
