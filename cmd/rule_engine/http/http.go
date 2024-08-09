package http

import (
	"log"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/limiter"
	"github.com/harishhary/blink/cmd/rule_engine/internal/message"
	"github.com/harishhary/blink/internal/configuration"
	"github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/messaging"
)

type HttpService struct {
	context.ServiceContext
	syncMessages messaging.MessageQueue
}

func New() *HttpService {
	serviceContext := context.New("BLINK-NODE - HTTP")
	if err := configuration.LoadFromEnvironment(&serviceContext); err != nil {
		log.Fatalln(err)
	}
	serviceContext.Logger = logger.New(serviceContext.Name(), "dev")

	return &HttpService{
		ServiceContext: serviceContext,
		syncMessages:   serviceContext.Messages().Subscribe(message.SyncService, false),
	}
}

func (service *HttpService) Run() errors.Error {
	// config := service.Configuration()
	app := fiber.New()

	app.Use(limiter.New(limiter.Config{
		Expiration: 10 * time.Second,
		Max:        3,
	}))

	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("Hello, World!")
	})
	if err := app.Listen(":3000"); err != nil {
		return errors.NewE(err)
	}
	return nil
}
