package plugin

import (
	"context"
	"fmt"
	"path/filepath"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/snapshot"
	"go.yaml.in/yaml/v4"
)

// ArtifactResolverMetaLifecycle describes the resolver meta-process lifecycle.
type ArtifactResolverMetaLifecycle string

const (
	ArtifactResolverMetaStarting   ArtifactResolverMetaLifecycle = "starting"
	ArtifactResolverMetaRunning    ArtifactResolverMetaLifecycle = "running"
	ArtifactResolverMetaRestarting ArtifactResolverMetaLifecycle = "restarting"
	ArtifactResolverMetaStopped    ArtifactResolverMetaLifecycle = "stopped"
)

// artifactResolverMetaStatus is owned by desiredStateReconcilerActor because the
// actor owns resolver generations, restart policy, and alias monitoring.
type artifactResolverMetaStatus struct {
	Lifecycle    ArtifactResolverMetaLifecycle
	Availability runtime.Availability
}

// artifactResolverMeta owns one resolver instance. It performs filesystem
// readiness checks and binary checksums for complete snapshot-resolution
// requests. The parent actor owns restart policy and fences results by alias.
type artifactResolverMeta struct {
	gen.MetaProcess
	directory string
	runCtx    context.Context
	cancelRun context.CancelFunc
	jobs      chan MessageResolveArtifacts
}

// --- messages ---

type MessageResolveArtifacts struct {
	snapshot snapshot.Snapshot
}

type MessageArtifactResolutionResult struct {
	source             gen.Alias
	snapshotGeneration int64
	desired            map[string]MessageApplyRouterDesiredState
	deferred           bool
	complete           bool
}

// --- messages ---

func (m *artifactResolverMeta) Init(process gen.MetaProcess) error {
	if m.directory == "" {
		return fmt.Errorf("artifact resolver meta: directory is required")
	}
	m.MetaProcess = process
	m.runCtx, m.cancelRun = context.WithCancel(context.Background())
	m.jobs = make(chan MessageResolveArtifacts, 1)
	return nil
}

func (m *artifactResolverMeta) Start() error {
	for {
		select {
		case <-m.runCtx.Done():
			return nil
		case request := <-m.jobs:
			desired, deferred, complete := m.buildDesiredRoutes(request.snapshot)
			if err := m.Send(m.Parent(), MessageArtifactResolutionResult{
				source:             m.ID(),
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

func (m *artifactResolverMeta) HandleMessage(_ gen.PID, message any) error {
	request, ok := message.(MessageResolveArtifacts)
	if !ok {
		return nil
	}
	select {
	case m.jobs <- request:
		return nil
	default:
		return fmt.Errorf("%w: request already queued", runtime.ErrArtifactResolve)
	}
}

func (m *artifactResolverMeta) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("actorruntime: unsupported artifact resolver call %T", request), nil
}

func (m *artifactResolverMeta) Terminate(error) {
	if m.cancelRun != nil {
		m.cancelRun()
	}
}

func (m *artifactResolverMeta) HandleInspect(gen.PID, ...string) map[string]string { return nil }

func (m *artifactResolverMeta) buildDesiredRoutes(snap snapshot.Snapshot) (map[string]MessageApplyRouterDesiredState, bool, bool) {
	desired := make(map[string]MessageApplyRouterDesiredState)

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
	return desired, deferred, true
}

func (m *artifactResolverMeta) resolveDeployment(entry snapshot.EffectiveEntry, ref *snapshot.ArtifactRef) (*Deployment, bool) {
	if ref == nil || !entry.Enabled {
		return nil, false
	}
	if ref.Name == "" || filepath.Base(ref.Name) != ref.Name || !filepath.IsLocal(ref.Name) || ref.Hash == "" {
		return nil, true
	}

	var metadata PluginMetadata
	if err := yaml.Unmarshal(ref.Spec, &metadata); err != nil || !metadata.Enabled || metadata.Id != entry.Id {
		return nil, true
	}

	path := filepath.Join(m.directory, ref.Name)
	digest, err := helpers.BinaryChecksum(path)
	if err != nil || ref.Hash != digest {
		return nil, true
	}

	return &Deployment{
		Id:         entry.Id,
		Name:       ref.Name,
		Enabled:    metadata.Enabled,
		Mode:       ref.RolloutMode,
		RolloutPct: metadata.RolloutPct,
		MaxProcs:   metadata.MaxProcs,
		Path:       path,
		Hash:       digest,
		Spec:       append([]byte(nil), ref.Spec...),
	}, false
}
