package plugin

import (
	"context"
	"crypto/sha256"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/runtime"
)

// Deployment describes one deployed plugin artifact.
type Deployment struct {
	Id         string
	Name       string
	Enabled    bool
	Mode       runtime.RolloutMode
	RolloutPct float64
	MinProcs   int
	MaxProcs   int
	Path       string
	Hash       string
	Spec       []byte
}

// DeploymentPoolKey identifies a pool for a plugin deployment.
type DeploymentPoolKey struct {
	runtime.PoolKey
	MinProcs int
	MaxProcs int
	SpecHash [sha256.Size]byte
}

// PoolKey returns the pool key for the deployment.
func (d *Deployment) PoolKey() DeploymentPoolKey {
	return DeploymentPoolKey{
		PoolKey:  runtime.PoolKey{Id: d.Id, Name: d.Name, Hash: d.Hash},
		MinProcs: d.MinProcs,
		MaxProcs: d.WorkerCount(),
		SpecHash: sha256.Sum256(d.Spec),
	}
}

// WorkerCount returns the number of workers for the deployment.
func (d *Deployment) WorkerCount() int {
	return max(1, d.MaxProcs)
}

// MessageDrain requests that an actor drain its work.
type MessageDrain struct{}

// MessageStop requests that an actor stop.
type MessageStop struct{}

// MessageInvokePlugin requests a plugin invocation.
type MessageInvokePlugin[T Artifact] struct {
	CallID     uint64
	Context    context.Context
	Cancel     context.CancelFunc
	PluginID   string
	RolloutKey string
	Fn         func(context.Context, T) error
	Shadow     bool
}

// MessageCancelInvocation requests cancellation of an invocation.
type MessageCancelInvocation struct {
	CallID uint64
	Err    error
}

// MessageInvocationCompleted reports an invocation result.
type MessageInvocationCompleted struct {
	CallID  uint64
	Err     error
	Route   gen.Atom
	Manager gen.PID
}
