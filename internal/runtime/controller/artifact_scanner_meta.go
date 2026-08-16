package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ergo.services/ergo/gen"
	"github.com/fsnotify/fsnotify"
	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/internal/snapshot"
)

const (
	scannerDebounce = 400 * time.Millisecond
	scannerPoll     = 5 * time.Second
)

// ArtifactScannerMetaLifecycle describes the controller-owned scanner meta lifecycle.
type ArtifactScannerMetaLifecycle string

const (
	ArtifactScannerMetaStarting   ArtifactScannerMetaLifecycle = "starting"
	ArtifactScannerMetaRunning    ArtifactScannerMetaLifecycle = "running"
	ArtifactScannerMetaRestarting ArtifactScannerMetaLifecycle = "restarting"
	ArtifactScannerMetaStopped    ArtifactScannerMetaLifecycle = "stopped"
)

// ArtifactScannerMetaStatus is derived and owned by the controller actor.
type ArtifactScannerMetaStatus struct {
	Lifecycle    ArtifactScannerMetaLifecycle
	Availability runtime.Availability
	Complete     bool
	LastError    error
}

// artifactScannerMeta owns filesystem observation and parsing for one scanner instance.
type artifactScannerMeta[T plugin.Syncable] struct {
	gen.MetaProcess
	directory string
	loader    plugin.Loader[T]
	parsed    map[string]T
	digests   map[string]string
	runCtx    context.Context
	cancelRun context.CancelFunc
}

// --- messages ---

type MessageArtifactScanResult struct {
	source     gen.Alias
	complete   bool
	entries    []snapshot.EffectiveEntry
	presentIDs []string
	err        error
}

// --- messages ---

// Init prepares scanner state and its cancellation context.
func (m *artifactScannerMeta[T]) Init(process gen.MetaProcess) error {
	if m.directory == "" || m.loader == nil {
		return fmt.Errorf("artifact scanner meta: directory and loader are required")
	}
	m.MetaProcess = process
	m.runCtx, m.cancelRun = context.WithCancel(context.Background())
	m.parsed = make(map[string]T)
	m.digests = make(map[string]string)
	m.Log().Debug("artifact scanner initialized: directory=%q alias=%s", m.directory, m.ID())
	return nil
}

// Start watches and periodically scans the artifact directory.
func (m *artifactScannerMeta[T]) Start() (runErr error) {
	defer func() {
		if runErr != nil {
			m.Log().Error("artifact scanner stopped: alias=%s error=%v", m.ID(), runErr)
			return
		}
		m.Log().Info("artifact scanner stopped: alias=%s", m.ID())
	}()

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("%w: create watcher: %w", runtime.ErrArtifactWatch, err)
	}
	defer watcher.Close()
	if m.runCtx.Err() != nil {
		return nil
	}
	if err := m.sendScan(watcher); err != nil {
		return err
	}
	m.Log().Info("artifact scanner started: directory=%q alias=%s parsed=%d binaries=%d", m.directory, m.ID(), len(m.parsed), len(m.digests))

	poll := time.NewTicker(scannerPoll)
	defer poll.Stop()
	var debounce *time.Timer
	var debounceC <-chan time.Time
	defer func() {
		if debounce != nil {
			debounce.Stop()
		}
	}()

	schedule := func() {
		if debounce == nil {
			debounce = time.NewTimer(scannerDebounce)
			debounceC = debounce.C
			return
		}
		if !debounce.Stop() {
			select {
			case <-debounce.C:
			default:
			}
		}
		debounce.Reset(scannerDebounce)
		debounceC = debounce.C
	}

	for {
		select {
		case <-m.runCtx.Done():
			return nil
		case event, ok := <-watcher.Events:
			if !ok {
				if m.runCtx.Err() != nil {
					return nil
				}
				return fmt.Errorf("%w: events closed", runtime.ErrArtifactWatch)
			}
			m.Log().Debug("artifact change detected: directory=%q alias=%s path=%q operation=%s", m.directory, m.ID(), event.Name, event.Op)
			schedule()
		case err, ok := <-watcher.Errors:
			if !ok {
				if m.runCtx.Err() != nil {
					return nil
				}
				return fmt.Errorf("%w: errors closed", runtime.ErrArtifactWatch)
			}
			return fmt.Errorf("%w: %w", runtime.ErrArtifactWatch, err)
		case <-debounceC:
			debounceC = nil
			m.Log().Debug("artifact changes detected; rescanning: directory=%q alias=%s", m.directory, m.ID())
			if err := m.sendScan(watcher); err != nil {
				return err
			}
		case <-poll.C:
			if err := m.sendScan(watcher); err != nil {
				return err
			}
		}
	}
}

// HandleMessage ignores messages because scans run in Start.
func (m *artifactScannerMeta[T]) HandleMessage(gen.PID, any) error { return nil }

// HandleCall rejects unsupported scanner calls.
func (m *artifactScannerMeta[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("artifact scanner meta: unsupported call %T", request), nil
}

// HandleInspect provides no custom scanner diagnostics.
func (m *artifactScannerMeta[T]) HandleInspect(gen.PID, ...string) map[string]string { return nil }

// Terminate cancels active filesystem observation.
func (m *artifactScannerMeta[T]) Terminate(error) {
	if m.cancelRun != nil {
		m.cancelRun()
	}
}

// sendScan observes the directory and forwards its effective catalog.
func (m *artifactScannerMeta[T]) sendScan(watcher *fsnotify.Watcher) error {
	attachErr := watcher.Add(m.directory)
	if attachErr != nil && strings.Contains(attachErr.Error(), "exists") {
		attachErr = nil
	}
	entries, ids, complete, err := m.scan()
	if err == nil && attachErr != nil {
		err = fmt.Errorf("%w: directory %q: %w", runtime.ErrArtifactWatch, m.directory, attachErr)
	}
	if err != nil {
		m.Log().Warning("artifact scan incomplete: directory=%q alias=%s error=%v", m.directory, m.ID(), err)
	}
	entries = cloneEntries(entries)
	if sendErr := m.Send(m.Parent(), MessageArtifactScanResult{
		source:     m.ID(),
		complete:   complete,
		entries:    entries,
		presentIDs: ids,
		err:        err,
	}); sendErr != nil {
		err := fmt.Errorf("artifact scanner meta: send scan: %w", sendErr)
		m.Log().Error("artifact scan result delivery failed: alias=%s error=%v", m.ID(), err)
		return err
	}
	if err == nil {
		m.Log().Debug("artifact scan complete: directory=%q alias=%s entries=%d ids=%d parsed=%d binaries=%d", m.directory, m.ID(), len(entries), len(ids), len(m.parsed), len(m.digests))
	}
	return nil
}

// scan retains last-good artifacts unless the directory itself cannot be read.
func (m *artifactScannerMeta[T]) scan() ([]snapshot.EffectiveEntry, []string, bool, error) {
	files, err := os.ReadDir(m.directory)
	if err != nil {
		return nil, nil, false, fmt.Errorf("%w: directory %q: %w", runtime.ErrArtifactScan, m.directory, err)
	}
	seenParsed := make(map[string]struct{})
	seenBinaries := make(map[string]struct{})
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		name := file.Name()
		path := filepath.Join(m.directory, name)
		if isYAML(name) {
			seenParsed[path] = struct{}{}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				m.Log().Debug("artifact spec read failed: path=%q error=%v", path, readErr)
				continue
			}
			item, parseErr := m.loader.ParseSpec(strings.TrimSuffix(name, filepath.Ext(name)), data)
			if parseErr != nil {
				m.Log().Debug("artifact spec parse failed: path=%q error=%v", path, parseErr)
				continue
			}
			m.parsed[path] = item
			metadata := item.Metadata()
			m.Log().Debug("artifact spec parsed: path=%q id=%q name=%q version=%q enabled=%t mode=%s", path, metadata.Id, metadata.Name, metadata.Version, metadata.Enabled, metadata.RolloutMode)
			continue
		}
		info, infoErr := file.Info()
		if infoErr != nil {
			m.Log().Debug("artifact binary stat failed: path=%q error=%v", path, infoErr)
			if _, known := m.digests[name]; known {
				// Preserve a known digest only while metadata for a still-present
				// file cannot be read. A successful non-executable stat is a delete.
				seenBinaries[name] = struct{}{}
			}
			continue
		}
		if info.Mode()&0111 == 0 {
			continue
		}
		seenBinaries[name] = struct{}{}
		digest, digestErr := helpers.BinaryChecksum(path)
		if digestErr != nil {
			m.Log().Debug("artifact binary checksum failed: path=%q error=%v", path, digestErr)
			continue
		}
		m.digests[name] = digest
		m.Log().Debug("artifact binary indexed: path=%q name=%q", path, name)
	}
	for path := range m.parsed {
		if _, ok := seenParsed[path]; !ok {
			delete(m.parsed, path)
		}
	}
	for name := range m.digests {
		if _, ok := seenBinaries[name]; !ok {
			delete(m.digests, name)
		}
	}

	groups := make(map[string][]T)
	ids := make([]string, 0, len(m.parsed))
	idSet := make(map[string]struct{})
	for _, item := range m.parsed {
		id := item.Metadata().Id
		groups[id] = append(groups[id], item)
		if _, ok := idSet[id]; !ok {
			idSet[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	entries := make([]snapshot.EffectiveEntry, 0, len(groups))
	for _, id := range ids {
		groupItems := groups[id]
		sort.Slice(groupItems, func(i, j int) bool { return groupItems[i].Metadata().Name < groupItems[j].Metadata().Name })
		paired := groupItems[:0]
		for _, item := range groupItems {
			metadata := item.Metadata()
			if !metadata.Enabled || m.digests[metadata.Name] != "" {
				paired = append(paired, item)
			}
		}
		group := CatalogGroup[T]{Id: id, Entries: paired}
		if len(paired) == 0 || len(ValidateGroup(group)) != 0 {
			continue
		}
		entries = append(entries, ElectGroup(id, group, m.digests))
	}
	return entries, ids, true, nil
}

// isYAML reports whether name has a supported YAML extension.
func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}
