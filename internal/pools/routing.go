package pools

import "sync"

// PluginRouting holds the routing configuration for a single plugin.
type PluginRouting struct {
	KillSwitch bool
	Mode       RolloutMode
	RolloutPct float64
}

// RoutingTable is a thread-safe map from pluginID to PluginRouting.
// Pass RoutingTable.Config() to NewProcessPool to enable live routing control.
// An empty table is valid - missing entries default to blue-green with no kill switch.
type RoutingTable struct {
	mu      sync.RWMutex
	entries map[string]PluginRouting
}

// Creates an empty RoutingTable.
func NewRoutingTable() *RoutingTable {
	return &RoutingTable{entries: make(map[string]PluginRouting)}
}

// Updates the routing config for pluginID. Reflected immediately on the next Call.
func (t *RoutingTable) Set(pluginID string, r PluginRouting) {
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
	return func(pluginID string) (bool, RolloutMode, float64) {
		t.mu.RLock()
		r, ok := t.entries[pluginID]
		t.mu.RUnlock()
		if !ok {
			return false, RolloutModeBlueGreen, 0
		}
		return r.KillSwitch, r.Mode, r.RolloutPct
	}
}
