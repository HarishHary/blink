package plugin

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/snapshot"
	"go.yaml.in/yaml/v4"
)

// ---------------------------------------------------------------------------
// Types & state
// ---------------------------------------------------------------------------

var ErrArtifactResolve = errors.New("plugin artifact resolution failed")

// ArtifactResolverMetaLifecycle describes the resolver meta-process lifecycle.
type ArtifactResolverMetaLifecycle string

const (
	ArtifactResolverMetaStarting   ArtifactResolverMetaLifecycle = "starting"
	ArtifactResolverMetaRunning    ArtifactResolverMetaLifecycle = "running"
	ArtifactResolverMetaRestarting ArtifactResolverMetaLifecycle = "restarting"
	ArtifactResolverMetaStopped    ArtifactResolverMetaLifecycle = "stopped"
)

// artifactResolverMetaState tracks the resolver meta-process state and restart policy.
type artifactResolverMetaState struct {
	alias   gen.Alias
	restart *runtime.ScheduledBackoff
	status  artifactResolverMetaStatus
}

// artifactResolverMetaStatus is owned by reconcilerActor, which owns generations and restart policy.
type artifactResolverMetaStatus struct {
	lifecycle    ArtifactResolverMetaLifecycle
	availability runtime.Availability
	LastError    error
}

// artifactResolverMeta owns one resolver, checking filesystem readiness and binary checksums for a
// whole snapshot; its parent fences results by alias.
type artifactResolverMeta struct {
	gen.MetaProcess
	directory string
	runCtx    context.Context
	cancelRun context.CancelFunc
	jobs      chan MessageResolveArtifacts
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// MessageResolveArtifacts requests resolution for a snapshot.
type MessageResolveArtifacts struct {
	snapshot snapshot.Snapshot
}

// MessageArtifactResolutionResult reports resolved desired routes for a snapshot.
type MessageArtifactResolutionResult struct {
	source             gen.Alias
	snapshotGeneration int64
	desired            map[string]routerDesiredState
	deferred           bool
	LastError          error
}

// ---------------------------------------------------------------------------
// Actor lifecycle & handlers
// ---------------------------------------------------------------------------

// Init initializes the artifact resolver meta-process.
func (m *artifactResolverMeta) Init(process gen.MetaProcess) error {
	if m.directory == "" {
		return fmt.Errorf("artifact resolver meta: directory is required")
	}
	m.MetaProcess = process
	m.runCtx, m.cancelRun = context.WithCancel(context.Background())
	m.jobs = make(chan MessageResolveArtifacts, 1)
	return nil
}

// Start resolves queued artifact requests until termination.
func (m *artifactResolverMeta) Start() error {
	for {
		select {
		case <-m.runCtx.Done():
			return nil
		case request := <-m.jobs:
			desired, deferred, resolveErr := m.buildDesiredRoutes(request.snapshot)
			if err := m.SendWithPriority(m.Parent(), MessageArtifactResolutionResult{
				source:             m.ID(),
				snapshotGeneration: request.snapshot.Generation,
				desired:            desired,
				deferred:           deferred,
				LastError:          resolveErr,
			}, gen.MessagePriorityHigh); err != nil {
				return fmt.Errorf("%w: send result: %w", ErrArtifactResolve, err)
			}
		}
	}
}

// Terminate cancels the resolver's running context.
func (m *artifactResolverMeta) Terminate(error) {
	if m.cancelRun != nil {
		m.cancelRun()
	}
}

// HandleMessage queues artifact resolution requests.
func (m *artifactResolverMeta) HandleMessage(_ gen.PID, message any) error {
	request, ok := message.(MessageResolveArtifacts)
	if !ok {
		return nil
	}
	select {
	case m.jobs <- request:
		return nil
	default:
		return fmt.Errorf("%w: request already queued", ErrArtifactResolve)
	}
}

// HandleCall rejects synchronous artifact resolver calls.
func (m *artifactResolverMeta) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("unsupported artifact resolver call %T", request), nil
}

// HandleInspect exposes the resolver's directory and job queue depth, which is 0 or 1.
func (m *artifactResolverMeta) HandleInspect(gen.PID, ...string) map[string]string {
	return map[string]string{
		"resolver:directory": m.directory,
		"resolver:queue":     fmt.Sprintf("%d/%d", len(m.jobs), cap(m.jobs)),
	}
}

// ---------------------------------------------------------------------------
// Work
// ---------------------------------------------------------------------------

// buildDesiredRoutes resolves all enabled snapshot entries into desired routes.
func (m *artifactResolverMeta) buildDesiredRoutes(snap snapshot.Snapshot) (map[string]routerDesiredState, bool, error) {
	desired := make(map[string]routerDesiredState)

	deferred := false
	var resolveErr error
	for _, entry := range snap.Entries {
		if !entry.Enabled {
			continue
		}
		route := routerDesiredState{}
		var primaryErr, candidateErr error
		route.primary, route.primaryDeferred, primaryErr = m.resolveDeployment(entry, entry.Primary)
		route.candidate, route.candidateDeferred, candidateErr = m.resolveDeployment(entry, entry.Candidate)
		resolveErr = runtime.FirstError(resolveErr, primaryErr, candidateErr)
		deferred = deferred || route.primaryDeferred || route.candidateDeferred
		desired[entry.Id] = route
	}
	return desired, deferred, resolveErr
}

// resolveDeployment validates and resolves an artifact reference into a deployment.
func (m *artifactResolverMeta) resolveDeployment(entry snapshot.EffectiveEntry, ref *snapshot.ArtifactRef) (*Deployment, bool, error) {
	if ref == nil || !entry.Enabled {
		return nil, false, nil
	}
	if ref.Name == "" || filepath.Base(ref.Name) != ref.Name || !filepath.IsLocal(ref.Name) || ref.Hash == "" {
		return nil, true, fmt.Errorf("%w: invalid artifact reference %q", ErrArtifactResolve, ref.Name)
	}

	var spec Spec
	if err := yaml.Unmarshal(ref.Spec, &spec); err != nil {
		return nil, true, fmt.Errorf("%w: parse %q: %w", ErrArtifactResolve, ref.Name, err)
	}
	if !spec.Enabled || spec.Id != entry.Id {
		return nil, true, fmt.Errorf("%w: invalid spec %q", ErrArtifactResolve, ref.Name)
	}

	path := filepath.Join(m.directory, ref.Name)
	digest, err := helpers.BinaryChecksum(path)
	if err != nil {
		return nil, true, fmt.Errorf("%w: checksum %q: %w", ErrArtifactResolve, ref.Name, err)
	}
	if ref.Hash != digest {
		return nil, true, fmt.Errorf("%w: %q: %w", ErrArtifactResolve, ref.Name, ErrArtifactMismatch)
	}

	return &Deployment{
		Id:                           entry.Id,
		Name:                         ref.Name,
		Enabled:                      spec.Enabled,
		Mode:                         ref.RolloutMode,
		RolloutPct:                   spec.RolloutPct,
		MinProcs:                     spec.MinProcs,
		MaxProcs:                     spec.MaxProcs,
		MaxConcurrentCallsPerProcess: spec.CallsPerProcess,
		Path:                         path,
		Hash:                         digest,
		Spec:                         append([]byte(nil), ref.Spec...),
	}, false, nil
}
