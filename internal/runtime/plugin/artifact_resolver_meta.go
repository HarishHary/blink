package plugin

import (
	"context"
	"fmt"
	"path/filepath"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/snapshot"
)

// ArtifactResolverLifecycle describes one resolver meta-process incarnation.
type ArtifactResolverLifecycle string

const (
	ArtifactResolverStarting   ArtifactResolverLifecycle = "starting"
	ArtifactResolverRunning    ArtifactResolverLifecycle = "running"
	ArtifactResolverRestarting ArtifactResolverLifecycle = "restarting"
	ArtifactResolverStopped    ArtifactResolverLifecycle = "stopped"
)

// ArtifactResolverStatus is owned by desiredStateReconcilerActor because the
// actor owns resolver generations, restart policy, and alias monitoring.
type ArtifactResolverStatus struct {
	Lifecycle      ArtifactResolverLifecycle
	Availability   runtime.Availability
	Incarnation    uint64
	RestartCount   uint64
	RestartPending bool
	LastError      error
}

// artifactResolverMeta owns one resolver incarnation. It performs filesystem
// readiness checks and binary checksums for complete snapshot-resolution
// requests. The parent actor owns restart policy and fences requests/results by
// incarnation.
type artifactResolverMeta[T Syncable] struct {
	gen.MetaProcess

	directory   string
	adapter     *PluginAdapter[T]
	incarnation uint64

	runCtx    context.Context
	cancelRun context.CancelFunc
	jobs      chan MessageResolveArtifacts
}

func (m *artifactResolverMeta[T]) Init(process gen.MetaProcess) error {
	if m.directory == "" || m.adapter == nil || m.adapter.Config == nil {
		return fmt.Errorf("artifact resolver meta: directory, adapter, and config are required")
	}
	m.MetaProcess = process
	m.runCtx, m.cancelRun = context.WithCancel(context.Background())
	m.jobs = make(chan MessageResolveArtifacts, 1)
	return nil
}

func (m *artifactResolverMeta[T]) Start() error {
	if err := m.Send(m.Parent(), MessageArtifactResolverStarted{incarnation: m.incarnation}); err != nil {
		return fmt.Errorf("%w: announce start: %w", runtime.ErrArtifactResolve, err)
	}

	for {
		select {
		case <-m.runCtx.Done():
			return nil
		case request := <-m.jobs:
			desired, deferred, complete := m.buildDesiredRoutes(request.snapshot)
			if err := m.Send(m.Parent(), MessageArtifactResolutionResult{
				incarnation:        m.incarnation,
				snapshotGeneration: request.snapshot.Generation,
				desired:            desired,
				deferred:           deferred,
				complete:           complete,
			}); err != nil {
				return fmt.Errorf("%w: send result: %w", runtime.ErrArtifactResolve, err)
			}
		}
	}
}

func (m *artifactResolverMeta[T]) HandleMessage(_ gen.PID, message any) error {
	request, ok := message.(MessageResolveArtifacts)
	if !ok || request.incarnation != m.incarnation {
		return nil
	}
	select {
	case m.jobs <- request:
		return nil
	default:
		return fmt.Errorf("%w: request already queued", runtime.ErrArtifactResolve)
	}
}

func (m *artifactResolverMeta[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return nil, fmt.Errorf("actorruntime: unsupported artifact resolver call %T", request)
}

func (m *artifactResolverMeta[T]) Terminate(error) {
	if m.cancelRun != nil {
		m.cancelRun()
	}
}

func (m *artifactResolverMeta[T]) HandleInspect(gen.PID, ...string) map[string]string {
	return map[string]string{
		"directory":   m.directory,
		"incarnation": fmt.Sprintf("%d", m.incarnation),
	}
}

func (m *artifactResolverMeta[T]) buildDesiredRoutes(snap snapshot.Snapshot) (map[string]MessageApplyRouterDesiredState, bool, bool) {
	desired := make(map[string]MessageApplyRouterDesiredState)
	if !m.configMatches(snap.Generation) {
		return desired, false, false
	}

	deferred := false
	for _, entry := range snap.Entries {
		if !entry.Enabled {
			continue
		}
		route := MessageApplyRouterDesiredState{}
		route.primary, route.primaryDeferred = m.resolveDeployment(entry, entry.Primary)
		route.candidate, route.candidateDeferred = m.resolveDeployment(entry, entry.Candidate)
		deferred = deferred || route.primaryDeferred || route.candidateDeferred
		desired[entry.Id] = route
	}
	if !m.configMatches(snap.Generation) {
		return nil, false, false
	}
	return desired, deferred, true
}

func (m *artifactResolverMeta[T]) resolveDeployment(entry snapshot.EffectiveEntry, ref *snapshot.ArtifactRef) (*Deployment, bool) {
	if ref == nil || !entry.Enabled {
		return nil, false
	}
	if ref.Name == "" || filepath.Base(ref.Name) != ref.Name || !filepath.IsLocal(ref.Name) || ref.Hash == "" {
		return nil, true
	}

	path := filepath.Join(m.directory, ref.Name)
	state, ok := m.adapter.Config.DesiredBinaryState(ref.Name)
	if !ok || !state.Enabled || state.Id != entry.Id {
		return nil, true
	}

	digest, err := helpers.BinaryChecksum(path)
	if err != nil || ref.Hash != digest {
		return nil, true
	}

	state.Id, state.Name = entry.Id, ref.Name
	return &Deployment{
		BinaryState: state,
		Path:        path,
		Hash:        digest,
		RolloutPct:  state.RolloutPct,
	}, false
}

func (m *artifactResolverMeta[T]) configMatches(generation int64) bool {
	source, ok := m.adapter.Config.(interface{ Generation() int64 })
	return !ok || source.Generation() == generation
}
