package pools

import "sync"

// RoutingEntry holds the routing configuration for a single plugin.
type RoutingEntry struct {
	Mode       RolloutMode
	RolloutPct float64
}

// RoutingTable is a thread-safe map from pluginID to RoutingEntry.
// Pass RoutingTable.Config() to NewProcessPool to enable live routing control.
// An empty table is valid - missing entries default to blue-green.
type RoutingTable struct {
	mu      sync.RWMutex
	entries map[string]RoutingEntry
}

// Creates an empty RoutingTable.
func NewRoutingTable() *RoutingTable {
	return &RoutingTable{entries: make(map[string]RoutingEntry)}
}

// Updates the routing config for pluginID. Reflected immediately on the next Call.
func (t *RoutingTable) Set(pluginID string, r RoutingEntry) {
	t.mu.Lock()
	t.entries[pluginID] = r
	t.mu.Unlock()
}

// Removes the routing config for pluginID, reverting it to blue-green defaults.
func (t *RoutingTable) Delete(pluginID string) {
	t.mu.Lock()
	delete(t.entries, pluginID)
	t.mu.Unlock()
}

// Returns a RoutingConfig closure that reads live from the table.
// Pass this to NewProcessPool.
func (t *RoutingTable) Config() RoutingConfig {
	return func(pluginID string) (RolloutMode, float64) {
		t.mu.RLock()
		r, ok := t.entries[pluginID]
		t.mu.RUnlock()
		if !ok {
			return RolloutModeBlueGreen, 0
		}
		return r.Mode, r.RolloutPct
	}
}
