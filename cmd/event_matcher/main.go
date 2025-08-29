package main

import (
	"github.com/harishhary/blink/cmd/event_matcher/matcher"
	"github.com/harishhary/blink/cmd/event_matcher/sync"
	"github.com/harishhary/blink/internal/services"
)

func main() {
   runner := services.New()
   runner.Register(
       sync.New(),
       matcher.New(),
   )

   runner.Run()
}
