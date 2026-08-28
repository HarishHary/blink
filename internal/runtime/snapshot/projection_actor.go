package snapshot

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/telemetry"
)

var (
	// ErrProjectionNotPrepared means the requested generation is not prepared.
	ErrProjectionNotPrepared = errors.New("snapshot projection: generation is not prepared")
)

// ProjectionCommitMode determines when a parsed snapshot becomes visible.
type ProjectionCommitMode uint8

const (
	// ProjectionCommitDirect makes each complete parsed snapshot visible immediately.
	ProjectionCommitDirect ProjectionCommitMode = iota
	// ProjectionCommitExternal prepares parsed snapshots until the bound coordinator activates one.
	ProjectionCommitExternal
)

// ProjectionActorLifecycle describes the projection actor's lifecycle.
type ProjectionActorLifecycle string

const (
	ProjectionActorStarting   ProjectionActorLifecycle = "starting"
	ProjectionActorRunning    ProjectionActorLifecycle = "running"
	ProjectionActorRestarting ProjectionActorLifecycle = "restarting"
	ProjectionActorStopped    ProjectionActorLifecycle = "stopped"
)

// ProjectionActorState tracks the active projection and its commit lifecycle.
type ProjectionActorState struct {
	CommittedGeneration int64
	ReadyGeneration     int64
	PendingGeneration   int64
	PendingPID          gen.PID
	Pid                 gen.PID
	Retry               *runtime.ScheduledBackoff
	DeadlineCancel      gen.CancelFunc
	DeadlineToken       uint64
	Status              ProjectionActorStatus
}

// ProjectionActorStatus is the projection actor's current runtime status.
type ProjectionActorStatus struct {
	Lifecycle           ProjectionActorLifecycle
	Availability        runtime.Availability
	CommittedGeneration int64
	PreparedGeneration  int64
}

// ProjectionState is an independently owned typed snapshot view.
type ProjectionState[T any] struct {
	ProjectionActorStatus
	ProjectionData[T]
}

// ProjectionData is the independently owned typed data parsed from a committed snapshot.
type ProjectionData[T any] struct {
	Primaries   []T
	Candidates  []T
	ByFileName  map[string]T
	RolloutByID map[string]Rollout
}

// Rollout is what a generation says about one plugin id's rollout; its zero value describes an id it
// does not carry.
type Rollout struct {
	MaxProcs        int
	CallsPerProcess int
	CanaryPct       float64
	Shadow          bool
}

// Capacity is the invocations this id's deployment can run at once: the two bounds multiplied, a
// ceiling not a promise.
func (r Rollout) Capacity() int {
	return max(1, r.MaxProcs) * max(1, r.CallsPerProcess)
}

// clone returns an independently owned copy of the projection data.
func (s ProjectionData[T]) clone(loader Loader[T]) ProjectionData[T] {
	clone := s
	clone.Primaries = cloneValues(s.Primaries, loader.Clone)
	clone.Candidates = cloneValues(s.Candidates, loader.Clone)
	clone.ByFileName = make(map[string]T, len(s.ByFileName))
	for name, value := range s.ByFileName {
		clone.ByFileName[name] = loader.Clone(value)
	}
	clone.RolloutByID = maps.Clone(s.RolloutByID)
	return clone
}

// cloneValues returns independently cloned values.
func cloneValues[T any](values []T, clone func(T) T) []T {
	valuesCopy := make([]T, len(values))
	for i, value := range values {
		valuesCopy[i] = clone(value)
	}
	return valuesCopy
}

type projectionActor[T any] struct {
	act.Actor
	snapshotEvent      gen.Event
	statusEvent        gen.Event
	loader             Loader[T]
	mode               ProjectionCommitMode
	readerActorReady   bool
	readerGeneration   int64
	observedGeneration int64
	committed          *parsedProjection[T]
	prepared           *parsedProjection[T]
	lastError          error
	lastStatus         ProjectionActorStatus
	labels             telemetry.Labels
}

// newProjectionActor constructs one subtree's typed view; it monitors both events and publishes
// through neither, so it takes names and no token.
func newProjectionActor[T any](snapshotEvent, statusEvent gen.Event, loader Loader[T], mode ProjectionCommitMode, labels telemetry.Labels) gen.ProcessBehavior {
	return &projectionActor[T]{snapshotEvent: snapshotEvent, statusEvent: statusEvent, loader: loader, mode: mode, labels: labels}
}

// --- messages ---

// ProjectionStateRequest reads the current immutable projection state.
type ProjectionStateRequest struct{}

// MessageProjectionActorStatusChanged reports projection status, with a zero PID from the child and
// stamped by Supervisor.
type MessageProjectionActorStatusChanged struct {
	Status        ProjectionActorStatus
	ProjectionPID gen.PID
}

// MessageProjectionCommit asks a Supervisor to commit a generation.
type MessageProjectionCommit struct {
	Generation    int64
	ProjectionPID gen.PID
}

// MessageProjectionCommitResult reports the result of a commit request.
type MessageProjectionCommitResult struct {
	Generation    int64
	ProjectionPID gen.PID
	Err           error
}

// MessageProjectionActorActivate tells a projection child its parent recorded its PID and it may
// monitor snapshot events.
type MessageProjectionActorActivate struct{}

// --- messages ---

// Init validates the projection actor's required events.
func (a *projectionActor[T]) Init(...any) error {
	if a.snapshotEvent.Name == "" || a.statusEvent.Name == "" {
		return fmt.Errorf("snapshot projection: snapshot events are required")
	}
	return nil
}

// HandleMessage starts event monitoring and processes projection commits.
func (a *projectionActor[T]) HandleMessage(from gen.PID, message any) error {
	switch m := message.(type) {
	case MessageProjectionActorActivate:
		if from != a.Parent() {
			return nil
		}
		for _, event := range []gen.Event{a.snapshotEvent, a.statusEvent} {
			buffered, err := a.MonitorEvent(event)
			if err != nil {
				return fmt.Errorf("monitor snapshot projection event %q: %w", event.Name, err)
			}
			for _, bufferedEvent := range buffered {
				if err := a.applyEvent(bufferedEvent); err != nil {
					return err
				}
			}
		}
		a.reconcileStatus()
	case MessageProjectionCommit:
		if from != a.Parent() {
			return nil
		}
		var err error
		switch {
		case a.mode != ProjectionCommitExternal:
			err = ErrProjectionNotPrepared
		case m.Generation != a.observedGeneration:
			err = fmt.Errorf("%w: %d", ErrProjectionNotPrepared, m.Generation)
		case a.committed == nil || a.committed.generation != m.Generation:
			if a.prepared == nil || a.prepared.generation != m.Generation {
				err = fmt.Errorf("%w: %d", ErrProjectionNotPrepared, m.Generation)
			} else {
				a.committed = a.prepared
				a.prepared = nil
				a.lastError = nil
			}
		}
		a.labels.Count(a, metricCommits, telemetry.Result(err))
		a.reconcileStatus()
		return a.Send(a.Parent(), MessageProjectionCommitResult{Generation: m.Generation, ProjectionPID: m.ProjectionPID, Err: err})
	case gen.MessageDownEvent:
		if m.Event == a.snapshotEvent || m.Event == a.statusEvent {
			return fmt.Errorf("snapshot projection event terminated: %w", m.Reason)
		}
	}
	return nil
}

// HandleEvent applies a monitored snapshot or reader status event.
func (a *projectionActor[T]) HandleEvent(event gen.MessageEvent) error {
	previousGeneration := a.observedGeneration
	err := a.applyEvent(event)
	if event.Event == a.snapshotEvent && a.observedGeneration > previousGeneration && a.lastError != nil {
		a.Log().Error("snapshot projection parse failed: generation=%d error=%v", a.observedGeneration, a.lastError)
	}
	a.reconcileStatus()
	return err
}

// HandleCall returns the current projection state.
func (a *projectionActor[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	switch request := request.(type) {
	case ProjectionStateRequest:
		return a.reportState(), nil
	default:
		return fmt.Errorf("snapshot projection: unsupported call %T", request), nil
	}
}

// applyEvent updates projection state from a monitored event.
func (a *projectionActor[T]) applyEvent(event gen.MessageEvent) error {
	switch event.Event {
	case a.snapshotEvent:
		snap, ok := event.Message.(*Snapshot)
		if !ok || snap == nil || snap.Generation <= a.observedGeneration {
			return nil
		}
		a.observedGeneration = snap.Generation
		a.prepared = nil
		start := time.Now()
		parsed, err := newParsedProjection(snap, a.loader)
		a.labels.Observe(a, metricParseTime, time.Since(start).Seconds())
		a.labels.Count(a, metricParses, parseResult(err, len(parsed.data.ByFileName)))
		if parsed.failures != 0 {
			a.labels.Add(a, metricParseFailures, float64(parsed.failures))
		}
		a.lastError = err
		// Nothing parsed leaves the previous generation standing; a partial one serves what parsed and
		// stays degraded.
		if err != nil && len(parsed.data.ByFileName) == 0 {
			return nil
		}
		if a.mode == ProjectionCommitExternal {
			a.prepared = &parsed
		} else {
			a.committed = &parsed
		}
	case a.statusEvent:
		status, ok := event.Message.(ReaderActorStatus)
		if ok {
			a.readerActorReady = status.Availability == runtime.AvailabilityReady
			a.readerGeneration = status.Generation
		}
	}
	return nil
}

// status derives the projection actor's current availability.
func (a *projectionActor[T]) status() ProjectionActorStatus {
	status := ProjectionActorStatus{Lifecycle: ProjectionActorRunning, Availability: runtime.AvailabilityUnavailable}
	if a.mode == ProjectionCommitExternal && a.prepared != nil {
		status.PreparedGeneration = a.prepared.generation
	}
	if a.committed == nil {
		return status
	}
	status.CommittedGeneration = a.committed.generation
	if a.lastError != nil {
		status.Availability = runtime.AvailabilityDegraded
		return status
	}
	if a.readerActorReady && a.readerGeneration >= a.committed.generation && a.observedGeneration >= a.committed.generation {
		status.Availability = runtime.AvailabilityReady
	}
	return status
}

// reportState returns an independently owned projection view.
func (a *projectionActor[T]) reportState() ProjectionState[T] {
	state := ProjectionState[T]{ProjectionActorStatus: a.status()}
	if a.committed == nil {
		return state
	}
	state.ProjectionData = a.committed.data.clone(a.loader)
	return state
}

// reconcileStatus recomputes and, on change, sends the current projection status to the supervisor
func (a *projectionActor[T]) reconcileStatus() {
	next := a.status()
	if next == a.lastStatus {
		return
	}
	a.lastStatus = next
	_ = a.Send(a.Parent(), MessageProjectionActorStatusChanged{Status: next})
}

// HandleInspect exposes lifecycle and availability plus the generation at each stage.
func (a *projectionActor[T]) HandleInspect(gen.PID, ...string) map[string]string {
	status := a.status()
	return map[string]string{
		"projection:lifecycle":            string(status.Lifecycle),
		"projection:availability":         string(status.Availability),
		"projection:committed_generation": fmt.Sprintf("%d", status.CommittedGeneration),
		"projection:prepared_generation":  fmt.Sprintf("%d", status.PreparedGeneration),
		"projection:observed_generation":  fmt.Sprintf("%d", a.observedGeneration),
		"projection:reader_ready":         fmt.Sprintf("%t", a.readerActorReady),
		"projection:reader_generation":    fmt.Sprintf("%d", a.readerGeneration),
	}
}

type parsedProjection[T any] struct {
	generation int64
	data       ProjectionData[T]
	failures   int
}

// parseResult grades a parse: a generation that lost some specs still serves, one that lost all of
// them does not.
func parseResult(err error, parsed int) string {
	switch {
	case err == nil:
		return "ok"
	case parsed != 0:
		return "partial"
	default:
		return "failed"
	}
}

// newParsedProjection converts a snapshot into owned typed data, skipping and joining specs that fail
// so one break costs itself.
func newParsedProjection[T any](snap *Snapshot, loader Loader[T]) (parsedProjection[T], error) {
	data := ProjectionData[T]{
		ByFileName:  make(map[string]T),
		RolloutByID: make(map[string]Rollout),
	}
	var parseErrs []error
	for _, entry := range snap.Entries {
		for index, ref := range []*ArtifactRef{entry.Primary, entry.Candidate} {
			if ref == nil || len(ref.Spec) == 0 {
				continue
			}
			value, err := loader.ParseSpec(ref.Name, append([]byte(nil), ref.Spec...))
			if err != nil {
				parseErrs = append(parseErrs, fmt.Errorf("parse snapshot spec %q (id %q): %w", ref.Name, entry.Id, err))
				continue
			}
			value = loader.Clone(value)
			data.ByFileName[ref.Name] = loader.Clone(value)
			rollout := data.RolloutByID[entry.Id]
			// Primary and candidate may declare different bounds and a call routes to either, so the id
			// carries the larger.
			rollout.MaxProcs = max(rollout.MaxProcs, 1, loader.MaxProcs(value))
			rollout.CallsPerProcess = max(rollout.CallsPerProcess, 1, loader.CallsPerProcess(value))
			if index == 0 {
				data.Primaries = append(data.Primaries, loader.Clone(value))
			} else {
				data.Candidates = append(data.Candidates, loader.Clone(value))
				rollout.Shadow = rollout.Shadow || ref.RolloutMode == runtime.RolloutModeShadow
				if ref.RolloutMode == runtime.RolloutModeCanary {
					rollout.CanaryPct = loader.RolloutPct(value)
				}
			}
			data.RolloutByID[entry.Id] = rollout
		}
	}
	return parsedProjection[T]{generation: snap.Generation, data: data, failures: len(parseErrs)}, errors.Join(parseErrs...)
}

// ProjectionClient performs bounded reads against the stable projection endpoint.
type ProjectionClient[T any] struct {
	node     gen.Node
	endpoint gen.ProcessID
}

// NewProjectionClient creates a client for the projection child of a namespace's subtree.
func NewProjectionClient[T any](node gen.Node, namespace string) *ProjectionClient[T] {
	return &ProjectionClient[T]{
		node:     node,
		endpoint: gen.ProcessID{Name: ProjectionActorName(namespace), Node: node.Name()},
	}
}

// State returns a deep-cloned typed snapshot state within Ergo's one-second call bound.
func (c *ProjectionClient[T]) State(ctx context.Context) (ProjectionState[T], error) {
	if err := ctx.Err(); err != nil {
		return ProjectionState[T]{}, err
	}
	response, err := c.node.CallProcessID(c.endpoint, ProjectionStateRequest{}, 1)
	if err != nil {
		return ProjectionState[T]{}, err
	}
	if err := ctx.Err(); err != nil {
		return ProjectionState[T]{}, err
	}
	state, ok := response.(ProjectionState[T])
	if !ok {
		return ProjectionState[T]{}, fmt.Errorf("snapshot projection: unexpected response %T", response)
	}
	return state, nil
}
