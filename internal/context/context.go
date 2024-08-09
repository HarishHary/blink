package context

import (
	"context"

	"github.com/harishhary/blink/internal/configuration"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/messaging"
	"google.golang.org/grpc/metadata"
)

type IContext interface {
	Configuration() configuration.ServiceConfiguration
	Messages() *messaging.Messages
	logger.ILogger
}

type ServiceContext struct {
	name     string
	messages *messaging.Messages
	configuration.ServiceConfiguration
	*logger.Logger
}

func New(name string) ServiceContext {
	return ServiceContext{
		name:     name,
		messages: messaging.New(),
	}
}

func (ctx *ServiceContext) Configuration() configuration.ServiceConfiguration {
	return ctx.ServiceConfiguration
}

func (ctx *ServiceContext) Messages() *messaging.Messages {
	return ctx.messages
}

func (ctx *ServiceContext) Name() string {
	return ctx.name
}

func (ctx *ServiceContext) GRPCContext() context.Context {
	return metadata.NewOutgoingContext(
		context.Background(),
		metadata.New(map[string]string{
			"token": ctx.Configuration().Token,
		}),
	)
}
