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
	// MaxConcurrentCallsPerProcess is how many invocations one plugin process may run at once, which
	// is what a deployment's process count no longer decides on its own: MinProcs and MaxProcs bound
	// the subprocesses that isolate a plugin, this bounds the throughput each one carries. One is
	// the default because it is the contract every plugin was written against - the plugin server
	// holds a single plugin object, so serialized calls are the only ones an arbitrary plugin can
	// assume - and raising it is per-deployment opt-in for a plugin whose code is concurrency-safe.
	MaxConcurrentCallsPerProcess int
	Path                         string
	Hash                         string
	Spec                         []byte
}

// DeploymentRouteKey identifies one concrete deployment: its artifact together with everything
// about it that the runtime cannot change underneath running processes. A route serves exactly one
// of these, so a deployment that alters any of them is a different route whose processes replace the
// old ones rather than a reconfiguration of the ones already running.
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

// MaxInvocationCapacity returns the deployment's own ceiling on concurrently executing invocations.
// It is a ceiling and not a promise: the running processes decide the capacity that exists now, and
// the process budget shared by every deployment decides how far this one may grow toward it.
func (d *Deployment) MaxInvocationCapacity() int {
	return d.ProcessCountLimit() * d.CapacityPerProcess()
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
