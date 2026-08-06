package plugin

import (
	"fmt"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/runtime"
)

// CatalogLifecycle describes one router-catalog actor incarnation.
type CatalogLifecycle string

const (
	CatalogStarting   CatalogLifecycle = "starting"
	CatalogRunning    CatalogLifecycle = "running"
	CatalogRestarting CatalogLifecycle = "restarting"
	CatalogDraining   CatalogLifecycle = "draining"
	CatalogStopped    CatalogLifecycle = "stopped"
)

// CatalogStatus is owned by catalogActor, except for RestartCount and
// ActorLastError, which are owned by runtimeSupervisor because the supervisor
// replaces catalog actor incarnations.
type CatalogStatus struct {
	Lifecycle          CatalogLifecycle
	Availability       runtime.Availability
	ActorGeneration    uint64
	ActorLastError     string
	RestartCount       uint64
	RestartPending     bool
	Revision           uint64
	DesiredRouters     int
	RoutableRouters    int
	DegradedRouters    int
	UnavailableRouters int
	Routers            map[string]RouterStatus
}

func (s CatalogStatus) clone() CatalogStatus {
	clone := s
	clone.Routers = make(map[string]RouterStatus, len(s.Routers))
	for id, status := range s.Routers {
		clone.Routers[id] = status.clone()
	}
	return clone
}

type routerState struct {
	pid             gen.PID
	actorGeneration uint64
	lastEpoch       uint64
	restartCount    uint64
	lastError       string
	restart         *scheduledBackoff
	status          RouterStatus
	retiring        bool
}

type catalogActor[T plugin.Syncable] struct {
	act.Actor
	deps            actorDependencies[T]
	actorGeneration uint64
	activated       bool
	routers         map[string]*routerState
	desired         map[string]MessageApplyRouterDesiredState
	inFlightCalls   map[uint64]gen.PID
	desiredRevision uint64
	draining        bool
	drainReported   bool
	liveStatus      CatalogStatus
	statusEpoch     uint64
}

type catalogActivate struct{ generation uint64 }

type MessageApplyCatalogDesiredState struct {
	desiredRevision uint64
	desired         map[string]MessageApplyRouterDesiredState
}

type catalogDrained struct {
	pid        gen.PID
	generation uint64
}

type catalogStatusChanged struct {
	pid        gen.PID
	generation uint64
	epoch      uint64
	status     CatalogStatus
}

type routerRestart struct {
	pluginID        string
	desiredRevision uint64
	token           uint64
}

func newCatalogActor[T plugin.Syncable](deps actorDependencies[T]) gen.ProcessBehavior {
	return &catalogActor[T]{deps: deps}
}

func (a *catalogActor[T]) Init(...any) error {
	a.routers = make(map[string]*routerState)
	a.desired = make(map[string]MessageApplyRouterDesiredState)
	a.inFlightCalls = make(map[uint64]gen.PID)
	return nil
}

func (a *catalogActor[T]) HandleMessage(_ gen.PID, message any) error {
	switch m := message.(type) {
	case catalogActivate:
		if m.generation <= a.actorGeneration {
			return nil
		}
		if a.activated {
			return fmt.Errorf(
				"catalog already activated as generation %d",
				a.actorGeneration,
			)
		}
		a.actorGeneration = m.generation
		a.activated = true
		a.reconcileStatus()

	case MessageApplyCatalogDesiredState:
		if !a.activated || a.draining || m.desiredRevision < a.desiredRevision {
			return nil
		}
		a.desiredRevision = m.desiredRevision
		a.desired = m.desired
		for id := range m.desired {
			a.sendDesiredToRouter(id)
		}
		for id, ref := range a.routers {
			if _, ok := m.desired[id]; ok {
				continue
			}
			ref.retiring = true
			a.cancelRouterRestart(id, false)
			if ref.pid == (gen.PID{}) {
				a.retireRouter(id, ErrPluginUnavailable)
				continue
			}
			_ = a.Send(ref.pid, drain{})
		}
		a.reconcileStatus()

	case invokeCall[T]:
		if a.draining || a.desiredRevision == 0 {
			a.finishUntrackedCall(m, ErrPluginUnavailable)
			return nil
		}
		ref := a.routers[m.pluginID]
		if ref == nil || ref.pid == (gen.PID{}) || ref.retiring {
			a.finishUntrackedCall(m, ErrPluginUnavailable)
			return nil
		}
		a.inFlightCalls[m.callID] = ref.pid
		if err := a.Send(ref.pid, m); err != nil {
			a.finishTrackedCall(m.callID, ErrPluginUnavailable)
			_ = a.Node().SendExit(ref.pid, fmt.Errorf("forward invocation to router: %w", err))
		}

	case cancelCall:
		routerPID, ok := a.inFlightCalls[m.callID]
		if !ok {
			return nil
		}
		if err := a.Send(routerPID, m); err != nil {
			a.finishTrackedCall(m.callID, m.err)
		}

	case callCompleted:
		if _, ok := a.inFlightCalls[m.callID]; ok {
			delete(a.inFlightCalls, m.callID)
			_ = a.Send(a.Parent(), m)
		}

	case drain:
		if a.draining {
			return nil
		}
		a.draining = true
		a.cancelAllRouterRestarts(false)
		a.reconcileStatus()
		if a.liveRouterCount() == 0 {
			a.reportDrained()
			return nil
		}
		for _, ref := range a.routers {
			if ref.pid == (gen.PID{}) {
				continue
			}
			ref.retiring = true
			_ = a.Send(ref.pid, drain{})
		}

	case routerDrained:
		ref := a.routers[m.pluginID]
		if ref == nil ||
			ref.pid != m.pid ||
			ref.actorGeneration != m.generation {
			return nil
		}
		if !a.draining && !ref.retiring {
			return nil
		}

		_ = a.Send(ref.pid, stop{})
		a.retireRouter(m.pluginID, ErrPluginUnavailable)

		if a.draining {
			a.reconcileStatus()
			if a.liveRouterCount() == 0 {
				a.reportDrained()
			}
			return nil
		}

		if _, desired := a.desired[m.pluginID]; desired {
			a.sendDesiredToRouter(m.pluginID)
		}
		a.reconcileStatus()

	case routerStatusChanged:
		ref := a.routers[m.pluginID]
		if ref == nil ||
			ref.pid != m.pid ||
			ref.actorGeneration != m.generation ||
			m.epoch <= ref.lastEpoch {
			return nil
		}
		ref.lastEpoch = m.epoch

		next := m.status.clone()
		next.ActorGeneration = ref.actorGeneration
		next.RestartCount = ref.restartCount
		next.RestartPending = a.routerRestartPending(m.pluginID)
		ref.lastError = ""
		a.resetRouterRestartBackoff(m.pluginID)
		next.ActorLastError = ref.lastError
		ref.status = next
		a.reconcileStatus()

	case routerRestart:
		ref := a.routers[m.pluginID]
		if ref == nil ||
			ref.restart == nil ||
			!ref.restart.pending ||
			ref.restart.token != m.token {
			return nil
		}
		ref.restart.pending = false
		ref.restart.cancel = nil
		ref.status.RestartPending = false
		if !a.draining && m.desiredRevision == a.desiredRevision {
			a.sendDesiredToRouter(m.pluginID)
		}

	case stop:
		return gen.TerminateReasonNormal

	case gen.MessageDownPID:
		for id, ref := range a.routers {
			if ref.pid != m.PID {
				continue
			}

			ref.restartCount++
			ref.lastError = errorText(m.Reason)
			ref.status.Lifecycle = RouterRestarting
			ref.status.Availability = runtime.AvailabilityUnavailable
			ref.status.ActorGeneration = ref.actorGeneration
			ref.status.RestartCount = ref.restartCount
			ref.status.RestartPending = false
			ref.status.ActorLastError = ref.lastError

			_, desired := a.desired[id]
			if a.draining || ref.retiring || !desired {
				a.retireRouter(id, ErrPluginUnavailable)
				if a.draining {
					if a.liveRouterCount() == 0 {
						a.reportDrained()
					}
				} else if desired {
					a.sendDesiredToRouter(id)
				}
			} else {
				ref.pid = gen.PID{}
				ref.lastEpoch = 0
				for callID, routerPID := range a.inFlightCalls {
					if routerPID == m.PID {
						a.finishTrackedCall(callID, ErrPluginUnavailable)
					}
				}
				_ = a.scheduleRouterRestart(id)
			}
			a.reconcileStatus()
			break
		}
	}
	return nil
}

func (a *catalogActor[T]) sendDesiredToRouter(id string) {
	desired, ok := a.desired[id]
	if !ok || a.draining {
		return
	}
	if ref := a.routers[id]; ref != nil && ref.pid != (gen.PID{}) && ref.retiring {
		return
	}
	ref, err := a.startRouter(id)
	if err != nil {
		_ = a.scheduleRouterRestart(id)
		return
	}
	ref.retiring = false
	desired.desiredRevision = a.desiredRevision
	if err := a.Send(ref.pid, desired); err != nil {
		_ = a.Node().SendExit(ref.pid, fmt.Errorf("apply desired state to router %q: %w", id, err))
	}
}

func (a *catalogActor[T]) startRouter(id string) (*routerState, error) {
	ref := a.routers[id]
	if ref != nil && ref.pid != (gen.PID{}) {
		return ref, nil
	}
	if ref == nil {
		ref = &routerState{}
		a.routers[id] = ref
	}

	ref.actorGeneration++
	generation := ref.actorGeneration
	ref.lastEpoch = 0
	ref.retiring = false
	ref.status = RouterStatus{
		Lifecycle:       RouterStarting,
		Availability:    runtime.AvailabilityUnavailable,
		ActorGeneration: generation,
		RestartCount:    ref.restartCount,
		ActorLastError:  ref.lastError,
		Primary: DeploymentPoolStatus{
			Lifecycle:    DeploymentPoolStopped,
			Availability: runtime.AvailabilityUnavailable,
			Workers:      make(map[int]PluginWorkerStatus),
		},
		Candidate: DeploymentPoolStatus{
			Lifecycle:    DeploymentPoolStopped,
			Availability: runtime.AvailabilityUnavailable,
			Workers:      make(map[int]PluginWorkerStatus),
		},
	}
	a.reconcileStatus()

	pid, err := a.Spawn(func() gen.ProcessBehavior {
		return &routerActor[T]{
			deps:     a.deps,
			pluginID: id,
		}
	}, gen.ProcessOptions{LinkParent: true})
	if err != nil {
		ref.restartCount++
		ref.status.RestartCount = ref.restartCount
		ref.status.Lifecycle = RouterRestarting
		ref.lastError = fmt.Sprintf("spawn router: %v", err)
		ref.status.ActorLastError = ref.lastError
		return ref, err
	}

	ref.pid = pid
	if err := a.MonitorPID(pid); err != nil {
		_ = a.Node().SendExit(pid, gen.TerminateReasonShutdown)
		ref.pid = gen.PID{}
		ref.restartCount++
		ref.status.RestartCount = ref.restartCount
		ref.lastError = fmt.Sprintf("monitor router: %v", err)
		ref.status.Lifecycle = RouterRestarting
		ref.status.ActorLastError = ref.lastError
		return ref, err
	}
	if err := a.Send(pid, routerActivate{generation: generation}); err != nil {
		_ = a.Node().SendExit(pid, fmt.Errorf("activate router: %w", err))
		return ref, err
	}
	return ref, nil
}

func (a *catalogActor[T]) retireRouter(id string, callErr error) {
	ref := a.routers[id]
	if ref == nil {
		return
	}

	if ref.restart != nil {
		ref.restart.cancelScheduled(false)
	}

	retiredPID := ref.pid
	if retiredPID != (gen.PID{}) {
		for callID, routerPID := range a.inFlightCalls {
			if routerPID == retiredPID {
				a.finishTrackedCall(callID, callErr)
			}
		}
	}

	ref.pid = gen.PID{}
	ref.lastEpoch = 0
	ref.retiring = false
	ref.status = RouterStatus{
		Lifecycle:       RouterStopped,
		Availability:    runtime.AvailabilityUnavailable,
		ActorGeneration: ref.actorGeneration,
		RestartCount:    ref.restartCount,
		RestartPending:  false,
		ActorLastError:  ref.lastError,
		Revision:        a.desiredRevision,
		Primary: DeploymentPoolStatus{
			Lifecycle:    DeploymentPoolStopped,
			Availability: runtime.AvailabilityUnavailable,
			Workers:      make(map[int]PluginWorkerStatus),
		},
		Candidate: DeploymentPoolStatus{
			Lifecycle:    DeploymentPoolStopped,
			Availability: runtime.AvailabilityUnavailable,
			Workers:      make(map[int]PluginWorkerStatus),
		},
	}
}

func (a *catalogActor[T]) routerRestartState(id string) *scheduledBackoff {
	ref := a.routers[id]
	if ref == nil {
		ref = &routerState{}
		a.routers[id] = ref
	}
	if ref.restart == nil {
		ref.restart = newScheduledBackoff(a.deps.retryMin, a.deps.retryMax)
	}
	return ref.restart
}

func (a *catalogActor[T]) scheduleRouterRestart(id string) error {
	if a.draining {
		return nil
	}
	state := a.routerRestartState(id)
	if state.pending {
		return nil
	}

	delay := state.strategy.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("router restart backoff stopped for %q", id)
	}
	state.token++
	token := state.token
	cancel, err := a.SendAfter(
		a.PID(),
		routerRestart{pluginID: id, desiredRevision: a.desiredRevision, token: token},
		delay,
	)
	if err != nil {
		return fmt.Errorf("schedule router restart for %q: %w", id, err)
	}
	state.pending = true
	state.cancel = cancel
	if ref := a.routers[id]; ref != nil {
		ref.status.Lifecycle = RouterRestarting
		ref.status.Availability = runtime.AvailabilityUnavailable
		ref.status.RestartPending = true
		ref.status.ActorLastError = ref.lastError
	}
	a.reconcileStatus()
	return nil
}

func (a *catalogActor[T]) cancelRouterRestart(id string, reset bool) {
	if ref := a.routers[id]; ref != nil {
		if ref.restart != nil {
			ref.restart.cancelScheduled(reset)
		}
		ref.status.RestartPending = false
	}
}

func (a *catalogActor[T]) resetRouterRestartBackoff(id string) {
	a.cancelRouterRestart(id, true)
}

func (a *catalogActor[T]) cancelAllRouterRestarts(reset bool) {
	for id := range a.routers {
		a.cancelRouterRestart(id, reset)
	}
}

func (a *catalogActor[T]) routerRestartPending(id string) bool {
	ref := a.routers[id]
	return ref != nil && ref.restart != nil && ref.restart.pending
}

func (a *catalogActor[T]) liveRouterCount() int {
	count := 0
	for _, ref := range a.routers {
		if ref.pid != (gen.PID{}) {
			count++
		}
	}
	return count
}

func (a *catalogActor[T]) finishUntrackedCall(call invokeCall[T], err error) {
	_ = a.Send(a.Parent(), callCompleted{callID: call.callID, err: err})
}

func (a *catalogActor[T]) finishTrackedCall(callID uint64, err error) {
	if _, ok := a.inFlightCalls[callID]; !ok {
		return
	}
	delete(a.inFlightCalls, callID)
	_ = a.Send(a.Parent(), callCompleted{callID: callID, err: err})
}

func (a *catalogActor[T]) reconcileStatus() {
	routers := make(map[string]RouterStatus, len(a.desired))
	routable := 0
	degraded := 0
	unavailable := 0

	for id := range a.desired {
		ref := a.routers[id]
		if ref == nil {
			unavailable++
			routers[id] = RouterStatus{
				Lifecycle:    RouterStarting,
				Availability: runtime.AvailabilityUnavailable,
			}
			continue
		}

		status := ref.status.clone()
		status.ActorGeneration = ref.actorGeneration
		status.RestartCount = ref.restartCount
		status.RestartPending = a.routerRestartPending(id)
		status.ActorLastError = ref.lastError
		routers[id] = status
		if status.NormalRoutable {
			routable++
		}
		switch status.Availability {
		case runtime.AvailabilityDegraded:
			degraded++
		case runtime.AvailabilityUnavailable:
			unavailable++
		}
	}

	lifecycle := CatalogStarting
	switch {
	case a.drainReported:
		lifecycle = CatalogStopped
	case a.draining:
		lifecycle = CatalogDraining
	case a.desiredRevision != 0:
		lifecycle = CatalogRunning
	}

	availability := runtime.AvailabilityUnavailable
	switch {
	case a.desiredRevision == 0 || len(a.desired) == 0:
		availability = runtime.AvailabilityUnavailable
	case unavailable == 0 && degraded == 0 && routable == len(a.desired):
		availability = runtime.AvailabilityReady
	case routable > 0:
		availability = runtime.AvailabilityDegraded
	}

	next := CatalogStatus{
		Lifecycle:          lifecycle,
		Availability:       availability,
		ActorGeneration:    a.actorGeneration,
		Revision:           a.desiredRevision,
		DesiredRouters:     len(a.desired),
		RoutableRouters:    routable,
		DegradedRouters:    degraded,
		UnavailableRouters: unavailable,
		Routers:            routers,
	}
	if sameCatalogStatus(a.liveStatus, next) && a.statusEpoch != 0 {
		return
	}

	a.statusEpoch++
	a.liveStatus = next
	if !a.activated {
		return
	}
	_ = a.Send(a.Parent(), catalogStatusChanged{
		pid:        a.PID(),
		generation: a.actorGeneration,
		epoch:      a.statusEpoch,
		status:     next.clone(),
	})
}

func sameCatalogStatus(left, right CatalogStatus) bool {
	if left.Lifecycle != right.Lifecycle ||
		left.Availability != right.Availability ||
		left.ActorGeneration != right.ActorGeneration ||
		left.Revision != right.Revision ||
		left.DesiredRouters != right.DesiredRouters ||
		left.RoutableRouters != right.RoutableRouters ||
		left.DegradedRouters != right.DegradedRouters ||
		left.UnavailableRouters != right.UnavailableRouters ||
		len(left.Routers) != len(right.Routers) {
		return false
	}
	for id, leftRouter := range left.Routers {
		rightRouter, ok := right.Routers[id]
		if !ok || !sameRouterStatus(leftRouter, rightRouter) {
			return false
		}
	}
	return true
}

func (a *catalogActor[T]) reportDrained() {
	if a.drainReported {
		return
	}
	a.drainReported = true
	a.reconcileStatus()
	_ = a.Send(a.Parent(), catalogDrained{
		pid:        a.PID(),
		generation: a.actorGeneration,
	})
}
