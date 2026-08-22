package plugin

import (
	"maps"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/runtime"
)

// ---------------------------------------------------------------------------
// Types & state
// ---------------------------------------------------------------------------

// DeploymentRouteLifecycle is the router- and catalog-facing lifecycle of one deployment route.
type DeploymentRouteLifecycle string

const (
	DeploymentRouteStarting   DeploymentRouteLifecycle = "starting"
	DeploymentRouteRunning    DeploymentRouteLifecycle = "running"
	DeploymentRouteRestarting DeploymentRouteLifecycle = "restarting"
	DeploymentRouteFailed     DeploymentRouteLifecycle = "failed"
	DeploymentRouteDraining   DeploymentRouteLifecycle = "draining"
	DeploymentRouteStopped    DeploymentRouteLifecycle = "stopped"
)

// deploymentRouteStatus is what one deployment route reports upward. It is a projection of a
// deployment manager's own status, not a separate actor's: the router keeps a route per plugin
// version, and the catalog and supervisor read process counts and queue depth from here.
type deploymentRouteStatus struct {
	lifecycle    DeploymentRouteLifecycle
	availability runtime.Availability
	readyProcs   int
	currentProcs int
	queueDepth   int
	activeCalls  int
	processes    map[gen.PID]pluginProcessStatus
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// clone copies a route status and its process map.
func (s deploymentRouteStatus) clone() deploymentRouteStatus {
	clone := s
	clone.processes = make(map[gen.PID]pluginProcessStatus, len(s.processes))
	maps.Copy(clone.processes, s.processes)
	return clone
}

// sameDeploymentRouteStatus compares route status snapshots.
func sameDeploymentRouteStatus(left, right deploymentRouteStatus) bool {
	if left.lifecycle != right.lifecycle ||
		left.availability != right.availability ||
		left.readyProcs != right.readyProcs ||
		left.currentProcs != right.currentProcs ||
		left.queueDepth != right.queueDepth ||
		left.activeCalls != right.activeCalls ||
		len(left.processes) != len(right.processes) {
		return false
	}
	for pid, process := range left.processes {
		other, ok := right.processes[pid]
		if !ok || !samePluginProcessStatus(process, other) {
			return false
		}
	}
	return true
}
