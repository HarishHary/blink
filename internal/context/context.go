package context

import (
	"context"

	"github.com/harishhary/blink/internal/configuration"
	"github.com/harishhary/blink/internal/logger"
	"google.golang.org/grpc/metadata"
)

type IContext interface {
	Configuration() configuration.ServiceConfiguration
	logger.ILogger
}

type ServiceContext struct {
	name string
	configuration.ServiceConfiguration
	*logger.Logger
}

func New(name string) ServiceContext {
	return ServiceContext{name: name}
}

func (ctx *ServiceContext) Configuration() configuration.ServiceConfiguration {
	return ctx.ServiceConfiguration
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
