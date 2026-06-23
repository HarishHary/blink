package config

import (
	"slices"
	"strings"

	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/pools"
)

// Registry indexes all YAML sidecars of a single plugin type in a directory.
type Registry[T plugin.Syncable] struct {
	byFileName map[string]T
	byID       map[string]T
	routing    map[string]pools.RoutingEntry
	all        []T
}

func (r *Registry[T]) All() []T                                 { return r.all }
func (r *Registry[T]) ByID(id string) (v T, ok bool)            { v, ok = r.byID[id]; return }
func (r *Registry[T]) ByFileName(name string) (v T, ok bool)    { v, ok = r.byFileName[name]; return }
func (r *Registry[T]) RoutingByID(id string) pools.RoutingEntry { return r.routing[id] }
func (r *Registry[T]) Len() int                                 { return len(r.all) }

// buildRegistry indexes a slice of already-loaded items. No file I/O.
// Items are sorted by name so Registry.All() returns a stable order.
func buildRegistry[T plugin.Syncable](items []T) *Registry[T] {
	reg := &Registry[T]{
		byFileName: make(map[string]T, len(items)),
		byID:       make(map[string]T, len(items)),
		routing:    make(map[string]pools.RoutingEntry, len(items)),
	}
	slices.SortFunc(items, func(a, b T) int {
		return strings.Compare(a.Metadata().Name, b.Metadata().Name)
	})
	for _, item := range items {
		m := item.Metadata()
		reg.byFileName[m.Name] = item
		reg.byID[m.Id] = item
		re := pools.RoutingEntry{Mode: m.RolloutMode, RolloutPct: m.RolloutPct}
		if existing, ok := reg.routing[m.Id]; ok {
			reg.routing[m.Id] = mergeRouting(existing, re)
		} else {
			reg.routing[m.Id] = re
		}
		reg.all = append(reg.all, item)
	}
	return reg
}

func mergeRouting(a, b pools.RoutingEntry) pools.RoutingEntry {
	out := a
	if b.Mode > a.Mode {
		out.Mode = b.Mode
	}
	if b.RolloutPct > a.RolloutPct {
		out.RolloutPct = b.RolloutPct
	}
	return out
}
