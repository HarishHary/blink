package plugin

import (
	"context"
	"crypto/sha256"
	"time"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/runtime"
)

type ActorDependencies[T Syncable] struct {
	Node           gen.Node
	Adapter        *PluginAdapter[T]
	QueueSize      int
	DrainTimeout   time.Duration
	HealthInterval time.Duration
	RetryMin       time.Duration
	RetryMax       time.Duration
	ControlTimeout time.Duration
}

type Deployment struct {
	Id         string
	Name       string
	Enabled    bool
	Mode       runtime.RolloutMode
	RolloutPct float64
	MaxProcs   int
	Path       string
	Hash       string
	Spec       []byte
}

type DeploymentPoolKey struct {
	runtime.PoolKey
	MaxProcs int
	SpecHash [sha256.Size]byte
}

func (d *Deployment) PoolKey() DeploymentPoolKey {
	return DeploymentPoolKey{
		PoolKey:  runtime.PoolKey{Id: d.Id, Name: d.Name, Hash: d.Hash},
		MaxProcs: d.WorkerCount(),
		SpecHash: sha256.Sum256(d.Spec),
	}
}

func (d *Deployment) WorkerCount() int {
	return max(1, d.MaxProcs)
}

type MessageDrain struct{}
type MessageStop struct{}

type MessageInvokePlugin[T Syncable] struct {
	CallID     uint64
	Context    context.Context
	Cancel     context.CancelFunc
	PluginID   string
	RolloutKey string
	Fn         func(context.Context, T) error
	Shadow     bool
}

type MessageCancelInvocation struct {
	CallID uint64
	Err    error
}

type MessageInvocationCompleted struct {
	CallID uint64
	Err    error
}
