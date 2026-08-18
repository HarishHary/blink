package snapshot

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/snapshot"
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

// ProjectionSpec supplies the type-specific behavior owned by a projection actor.
// Parse and Clone must not retain references to their inputs.
type ProjectionSpec[T any] struct {
	Parse    func(name string, spec []byte) (T, error)
	Clone    func(T) T
	MaxProcs func(T) int
}

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

// ProjectionData is the independently owned typed data from a snapshot.
type ProjectionData[T any] struct {
	Primaries    []T
	Candidates   []T
	ByFileName   map[string]T
	MaxProcsByID map[string]int
}

// clone returns an independently owned copy of the projection data.
func (s ProjectionData[T]) clone(spec ProjectionSpec[T]) ProjectionData[T] {
	clone := s
	clone.Primaries = cloneValues(s.Primaries, spec.Clone)
	clone.Candidates = cloneValues(s.Candidates, spec.Clone)
	clone.ByFileName = make(map[string]T, len(s.ByFileName))
	for name, value := range s.ByFileName {
		clone.ByFileName[name] = spec.Clone(value)
	}
	clone.MaxProcsByID = maps.Clone(s.MaxProcsByID)
	return clone
}

// ProjectionState is an independently owned typed snapshot view.
type ProjectionState[T any] struct {
	ProjectionActorStatus
	ProjectionData[T]
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
	events             Events
	spec               ProjectionSpec[T]
	mode               ProjectionCommitMode
	readerActorReady   bool
	readerGeneration   int64
	observedGeneration int64
	committed          *parsedProjection[T]
	prepared           *parsedProjection[T]
	lastError          error
}

// --- messages ---

// ProjectionStateRequest reads the current immutable projection state.
type ProjectionStateRequest struct{}

// MessageProjectionActorStatusChanged reports projection status. The child sends it
// to its parent with a zero PID; Supervisor stamps external reports.
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

// MessageProjectionActorActivate tells a projection child that its parent has recorded
// the child's PID and it may begin monitoring snapshot events.
type MessageProjectionActorActivate struct{}

// --- messages ---

// Init validates the projection actor's required events.
func (a *projectionActor[T]) Init(...any) error {
	if a.events.Snapshot.Name == "" || a.events.Status.Name == "" {
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
		for _, event := range []gen.Event{a.events.Snapshot, a.events.Status} {
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
		a.reportStatus()
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
		a.reportStatus()
		return a.Send(a.Parent(), MessageProjectionCommitResult{Generation: m.Generation, ProjectionPID: m.ProjectionPID, Err: err})
	case gen.MessageDownEvent:
		if m.Event == a.events.Snapshot || m.Event == a.events.Status {
			return fmt.Errorf("snapshot projection event terminated: %w", m.Reason)
		}
	}
	return nil
}

// HandleEvent applies a monitored snapshot or reader status event.
func (a *projectionActor[T]) HandleEvent(event gen.MessageEvent) error {
	previousGeneration := a.observedGeneration
	err := a.applyEvent(event)
	if event.Event == a.events.Snapshot && a.observedGeneration > previousGeneration && a.lastError != nil {
		a.Log().Error("snapshot projection parse failed: generation=%d error=%v", a.observedGeneration, a.lastError)
	}
	a.reportStatus()
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
	case a.events.Snapshot:
		snap, ok := event.Message.(*snapshot.Snapshot)
		if !ok || snap == nil || snap.Generation <= a.observedGeneration {
			return nil
		}
		a.observedGeneration = snap.Generation
		a.prepared = nil
		parsed, err := parseProjection(snap, a.spec)
		if err != nil {
			a.lastError = err
			return nil
		}
		if a.mode == ProjectionCommitExternal {
			a.prepared = &parsed
		} else {
			a.committed = &parsed
		}
		a.lastError = nil
	case a.events.Status:
		status, ok := event.Message.(ReaderActorStatus)
		if ok {
			a.readerActorReady = status.Availability == runtime.AvailabilityReady
			a.readerGeneration = status.Generation
		}
	}
	return nil
}

// currentStatus derives the projection actor's current availability.
func (a *projectionActor[T]) currentStatus() ProjectionActorStatus {
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
	state := ProjectionState[T]{ProjectionActorStatus: a.currentStatus()}
	if a.committed == nil {
		return state
	}
	state.ProjectionData = a.committed.data.clone(a.spec)
	return state
}

// reportStatus sends the current projection status to the supervisor.
func (a *projectionActor[T]) reportStatus() {
	_ = a.Send(a.Parent(), MessageProjectionActorStatusChanged{Status: a.currentStatus()})
}

type parsedProjection[T any] struct {
	generation int64
	data       ProjectionData[T]
}

// parseProjection converts a snapshot into independently owned typed data.
func parseProjection[T any](snap *snapshot.Snapshot, spec ProjectionSpec[T]) (parsedProjection[T], error) {
	data := ProjectionData[T]{
		ByFileName:   make(map[string]T),
		MaxProcsByID: make(map[string]int),
	}
	for _, entry := range snap.Entries {
		for index, ref := range []*snapshot.ArtifactRef{entry.Primary, entry.Candidate} {
			if ref == nil || len(ref.Spec) == 0 {
				continue
			}
			value, err := spec.Parse(ref.Name, append([]byte(nil), ref.Spec...))
			if err != nil {
				return parsedProjection[T]{}, fmt.Errorf("parse snapshot spec %q (id %q): %w", ref.Name, entry.Id, err)
			}
			value = spec.Clone(value)
			data.ByFileName[ref.Name] = spec.Clone(value)
			maxProcs := spec.MaxProcs(value)
			maxProcs = max(1, maxProcs)
			if maxProcs > data.MaxProcsByID[entry.Id] {
				data.MaxProcsByID[entry.Id] = maxProcs
			}
			if index == 0 {
				data.Primaries = append(data.Primaries, spec.Clone(value))
			} else {
				data.Candidates = append(data.Candidates, spec.Clone(value))
			}
		}
	}
	return parsedProjection[T]{generation: snap.Generation, data: data}, nil
}

// ProjectionClient performs bounded reads against the stable projection endpoint.
type ProjectionClient[T any] struct {
	node     gen.Node
	endpoint gen.ProcessID
}

// NewProjectionClient creates a client for name's supervised projection child.
func NewProjectionClient[T any](node gen.Node, name gen.Atom) *ProjectionClient[T] {
	return &ProjectionClient[T]{
		node:     node,
		endpoint: gen.ProcessID{Name: projectionActorName(name), Node: node.Name()},
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
