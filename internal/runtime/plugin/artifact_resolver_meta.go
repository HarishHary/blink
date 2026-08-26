package plugin

import (
	"context"
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

// artifactResolverMetaStatus is owned by reconcilerActor because the
// actor owns resolver generations, restart policy, and alias monitoring.
type artifactResolverMetaStatus struct {
	lifecycle    ArtifactResolverMetaLifecycle
	availability runtime.Availability
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
}

// ---------------------------------------------------------------------------
// Meta lifecycle
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
			desired, deferred := m.buildDesiredRoutes(request.snapshot)
			if err := m.Send(m.Parent(), MessageArtifactResolutionResult{
				source:             m.ID(),
				snapshotGeneration: request.snapshot.Generation,
				desired:            desired,
				deferred:           deferred,
			}); err != nil {
				return fmt.Errorf("%w: send result: %w", runtime.ErrArtifactResolve, err)
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

// ---------------------------------------------------------------------------
// Message handling
// ---------------------------------------------------------------------------

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
		return fmt.Errorf("%w: request already queued", runtime.ErrArtifactResolve)
	}
}

// HandleCall rejects synchronous artifact resolver calls.
func (m *artifactResolverMeta) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("unsupported artifact resolver call %T", request), nil
}

// HandleInspect exposes no inspect fields for the artifact resolver.
func (m *artifactResolverMeta) HandleInspect(gen.PID, ...string) map[string]string { return nil }

// ---------------------------------------------------------------------------
// Artifact resolution
// ---------------------------------------------------------------------------

// buildDesiredRoutes resolves all enabled snapshot entries into desired routes.
func (m *artifactResolverMeta) buildDesiredRoutes(snap snapshot.Snapshot) (map[string]routerDesiredState, bool) {
	desired := make(map[string]routerDesiredState)

	deferred := false
	for _, entry := range snap.Entries {
		if !entry.Enabled {
			continue
		}
		route := routerDesiredState{}
		route.primary, route.primaryDeferred = m.resolveDeployment(entry, entry.Primary)
		route.candidate, route.candidateDeferred = m.resolveDeployment(entry, entry.Candidate)
		deferred = deferred || route.primaryDeferred || route.candidateDeferred
		desired[entry.Id] = route
	}
	return desired, deferred
}

// resolveDeployment validates and resolves an artifact reference into a deployment.
func (m *artifactResolverMeta) resolveDeployment(entry snapshot.EffectiveEntry, ref *snapshot.ArtifactRef) (*Deployment, bool) {
	if ref == nil || !entry.Enabled {
		return nil, false
	}
	if ref.Name == "" || filepath.Base(ref.Name) != ref.Name || !filepath.IsLocal(ref.Name) || ref.Hash == "" {
		return nil, true
	}

	var spec Spec
	if err := yaml.Unmarshal(ref.Spec, &spec); err != nil || !spec.Enabled || spec.Id != entry.Id {
		return nil, true
	}

	path := filepath.Join(m.directory, ref.Name)
	digest, err := helpers.BinaryChecksum(path)
	if err != nil || ref.Hash != digest {
		return nil, true
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
	}, false
}
