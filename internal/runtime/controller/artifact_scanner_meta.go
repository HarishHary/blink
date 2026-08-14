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

// ArtifactScannerLifecycle describes the controller-owned scanner meta lifecycle.
type ArtifactScannerLifecycle string

const (
	ArtifactScannerStarting   ArtifactScannerLifecycle = "starting"
	ArtifactScannerRunning    ArtifactScannerLifecycle = "running"
	ArtifactScannerRestarting ArtifactScannerLifecycle = "restarting"
	ArtifactScannerStopped    ArtifactScannerLifecycle = "stopped"
)

// ArtifactScannerStatus is derived and owned by the controller actor.
type ArtifactScannerStatus struct {
	Lifecycle      ArtifactScannerLifecycle
	Availability   runtime.Availability
	Incarnation    uint64
	RestartCount   uint64
	RestartPending bool
	Complete       bool
	LastError      error
}

// artifactScannerMeta owns filesystem observation and parsing for one scanner incarnation.
type artifactScannerMeta[T plugin.Syncable] struct {
	gen.MetaProcess
	directory   string
	loader      plugin.Loader[T]
	incarnation uint64
	parsed      map[string]T
	digests     map[string]string
	runCtx      context.Context
	cancelRun   context.CancelFunc
}

// --- messages ---

type MessageArtifactScanResult struct {
	incarnation uint64
	complete    bool
	entries     []snapshot.EffectiveEntry
	presentIDs  []string
	err         error
}

// --- messages ---

func (m *artifactScannerMeta[T]) Init(process gen.MetaProcess) error {
	if m.directory == "" || m.loader == nil {
		return fmt.Errorf("artifact scanner meta: directory and loader are required")
	}
	m.MetaProcess = process
	m.runCtx, m.cancelRun = context.WithCancel(context.Background())
	m.parsed = make(map[string]T)
	m.digests = make(map[string]string)
	return nil
}

func (m *artifactScannerMeta[T]) Start() error {
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
		case _, ok := <-watcher.Events:
			if !ok {
				if m.runCtx.Err() != nil {
					return nil
				}
				return fmt.Errorf("%w: events closed", runtime.ErrArtifactWatch)
			}
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

func (m *artifactScannerMeta[T]) HandleMessage(gen.PID, any) error { return nil }

func (m *artifactScannerMeta[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return nil, fmt.Errorf("artifact scanner meta: unsupported call %T", request)
}

func (m *artifactScannerMeta[T]) Terminate(error) {
	if m.cancelRun != nil {
		m.cancelRun()
	}
}

func (m *artifactScannerMeta[T]) HandleInspect(gen.PID, ...string) map[string]string {
	return map[string]string{"incarnation": fmt.Sprintf("%d", m.incarnation)}
}

func (m *artifactScannerMeta[T]) sendScan(watcher *fsnotify.Watcher) error {
	attachErr := watcher.Add(m.directory)
	if attachErr != nil && strings.Contains(attachErr.Error(), "exists") {
		attachErr = nil
	}
	entries, ids, complete, err := m.scan()
	if err == nil && attachErr != nil {
		err = fmt.Errorf("%w: directory %q: %w", runtime.ErrArtifactWatch, m.directory, attachErr)
	}
	entries = cloneEntries(entries)
	if sendErr := m.Send(m.Parent(), MessageArtifactScanResult{
		incarnation: m.incarnation,
		complete:    complete,
		entries:     entries,
		presentIDs:  ids,
		err:         err,
	}); sendErr != nil {
		return fmt.Errorf("artifact scanner meta: send scan: %w", sendErr)
	}
	return nil
}

// scan keeps individually unreadable, unparseable, or unhashable files at their last-good state.
// Only ReadDir failure makes the scan incomplete.
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
				continue
			}
			item, parseErr := m.loader.ParseSpec(strings.TrimSuffix(name, filepath.Ext(name)), data)
			if parseErr != nil {
				continue
			}
			m.parsed[path] = item
			continue
		}
		info, infoErr := file.Info()
		if infoErr != nil {
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
		if digestErr == nil {
			m.digests[name] = digest
		}
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

func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}
