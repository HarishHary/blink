package plugin

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/internal/plugin"
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
	Generation     uint64
	RestartPending bool
	LastError      string
}

// artifactResolverMeta owns one resolver incarnation. It performs filesystem
// readiness checks and binary checksums for complete snapshot-resolution
// requests. The parent actor owns restart policy and fences requests/results by
// generation.
type artifactResolverMeta[T plugin.Syncable] struct {
	gen.MetaProcess

	directory  string
	adapter    *plugin.PluginAdapter[T]
	generation uint64

	runCtx    context.Context
	cancelRun context.CancelFunc
	closeOnce sync.Once
}

func (m *artifactResolverMeta[T]) Init(process gen.MetaProcess) error {
	m.MetaProcess = process
	m.runCtx, m.cancelRun = context.WithCancel(context.Background())
	return nil
}

func (m *artifactResolverMeta[T]) Start() error {
	if err := m.Send(m.Parent(), artifactResolverStarted{generation: m.generation}); err != nil {
		return fmt.Errorf("announce artifact resolver start: %w", err)
	}
	<-m.runCtx.Done()
	return nil
}

func (m *artifactResolverMeta[T]) HandleMessage(_ gen.PID, message any) error {
	switch request := message.(type) {
	case artifactResolve:
		if request.resolverGeneration != m.generation {
			return nil
		}
		desired, deferred := m.buildDesiredRoutes(request.snapshot)
		return m.Send(m.Parent(), artifactResolved{
			resolverGeneration: m.generation,
			id:                 request.id,
			snapshotGeneration: request.snapshotGeneration,
			desired:            desired,
			deferred:           deferred,
		})
	}
	return nil
}

func (m *artifactResolverMeta[T]) HandleCall(gen.PID, gen.Ref, any) (any, error) {
	return nil, nil
}

func (m *artifactResolverMeta[T]) Terminate(error) {
	m.closeOnce.Do(func() {
		if m.cancelRun != nil {
			m.cancelRun()
		}
	})
}

func (m *artifactResolverMeta[T]) HandleInspect(gen.PID, ...string) map[string]string {
	return map[string]string{
		"directory":  m.directory,
		"generation": fmt.Sprintf("%d", m.generation),
	}
}

func (m *artifactResolverMeta[T]) buildDesiredRoutes(snap *snapshot.Snapshot) (map[string]routerApplyDesired, bool) {
	desired := make(map[string]routerApplyDesired)
	if snap == nil {
		return desired, false
	}

	deferred := false
	for _, entry := range snap.Entries {
		route := routerApplyDesired{}
		route.primary, route.primaryDeferred = m.resolveDeployment(entry, entry.Primary)
		route.candidate, route.candidateDeferred = m.resolveDeployment(entry, entry.Candidate)
		deferred = deferred || route.primaryDeferred || route.candidateDeferred
		desired[entry.Id] = route
	}
	return desired, deferred
}

func (m *artifactResolverMeta[T]) resolveDeployment(entry snapshot.EffectiveEntry, ref *snapshot.ArtifactRef) (*deployment, bool) {
	if ref == nil || !entry.Enabled {
		return nil, false
	}

	path := filepath.Join(m.directory, ref.Name)
	state, ok := m.adapter.Config.DesiredBinaryState(ref.Name)
	if !ok || !state.Enabled || state.Id != entry.Id || !m.adapter.IsReady(path) {
		return nil, true
	}

	digest, err := helpers.BinaryChecksum(path)
	if err != nil || (ref.Hash != "" && ref.Hash != digest) {
		return nil, true
	}

	state.Id, state.Name = entry.Id, ref.Name
	rolloutPct := 0.0
	if source, ok := m.adapter.Config.(interface {
		ByFileName(string) (T, bool)
	}); ok {
		if item, found := source.ByFileName(ref.Name); found {
			rolloutPct = item.Metadata().RolloutPct
		}
	}
	return &deployment{
		BinaryState: state,
		path:        path,
		hash:        digest,
		rolloutPct:  rolloutPct,
	}, false
}
