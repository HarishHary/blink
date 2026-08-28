package plugin

import (
	"bytes"
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
	// MaxConcurrentCallsPerProcess is one process's throughput, where MinProcs and MaxProcs bound the
	// subprocesses; above 1 the plugin has to be concurrency-safe.
	MaxConcurrentCallsPerProcess int
	Path                         string
	Hash                         string
	Spec                         []byte
}

// DeploymentRouteKey identifies one concrete deployment: its artifact plus what the runtime cannot
// change under running processes, so altering any of it replaces them.
type DeploymentRouteKey struct {
	runtime.ArtifactKey
	MinProcs     int
	MaxProcs     int
	CallsPerProc int
	SpecHash     [sha256.Size]byte
}

// RouteKey returns the route identity of the deployment.
func (d *Deployment) RouteKey() DeploymentRouteKey {
	return DeploymentRouteKey{
		ArtifactKey:  runtime.ArtifactKey{Id: d.Id, Name: d.Name, Hash: d.Hash},
		MinProcs:     d.MinProcs,
		MaxProcs:     d.ProcessCountLimit(),
		CallsPerProc: d.CapacityPerProcess(),
		SpecHash:     sha256.Sum256(d.Spec),
	}
}

// ProcessCountLimit returns the most plugin processes the deployment may run at once.
func (d *Deployment) ProcessCountLimit() int {
	return max(1, d.MaxProcs)
}

// CapacityPerProcess returns how many invocations one of those processes may run at once.
func (d *Deployment) CapacityPerProcess() int {
	if d.MaxConcurrentCallsPerProcess <= 0 {
		return DefaultDeploymentCallsPerProcess
	}
	return d.MaxConcurrentCallsPerProcess
}

// MaxInvocationCapacity is the deployment's own ceiling on concurrent invocations, not a promise:
// running processes and the shared budget decide what exists.
func (d *Deployment) MaxInvocationCapacity() int {
	return d.ProcessCountLimit() * d.CapacityPerProcess()
}

// sameDeployment reports whether two deployments are field-for-field equal; a field added above
// belongs here too, or a real change reads as no change.
func sameDeployment(left, right *Deployment) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Id == right.Id &&
		left.Name == right.Name &&
		left.Enabled == right.Enabled &&
		left.Mode == right.Mode &&
		left.RolloutPct == right.RolloutPct &&
		left.MinProcs == right.MinProcs &&
		left.MaxProcs == right.MaxProcs &&
		left.MaxConcurrentCallsPerProcess == right.MaxConcurrentCallsPerProcess &&
		left.Path == right.Path &&
		left.Hash == right.Hash &&
		bytes.Equal(left.Spec, right.Spec)
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
