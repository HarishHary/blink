package main

import (
   "github.com/harishhary/blink/cmd/alert_formatter/sync"
   "github.com/harishhary/blink/cmd/alert_formatter/formatter"
   "github.com/harishhary/blink/internal/services"
)

func main() {
   runner := services.New()
   runner.Register(
       sync.New(),
       formatter.New(),
   )
   runner.Run()
}