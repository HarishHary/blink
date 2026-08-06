package backends

import "time"

// RecordStatus is the lifecycle state of a plugin in the controller's persistent store.
type RecordStatus string

const (
	// StatusActive - the plugin has a YAML sidecar; executors should consider it.
	StatusActive RecordStatus = "active"
	// StatusAbsent - the plugin was in YAML but is no longer present.
	// Kept in the store for history; not included in the effective snapshot.
	StatusAbsent RecordStatus = "absent"
)

// ControllerRecord is the persistent memory for one logical plugin ID - the control
// plane's history, owned by the store. YAML is the source of truth for authored spec;
// the store owns history and metadata. This lives with the persistence layer (backends),
// not with the wire model (internal/snapshot).
type ControllerRecord struct {
	Id               string
	FirstSeenAt      time.Time
	LastSeenAt       time.Time
	Status           RecordStatus
	ValidationErrors []string
}

// Clone returns a deep copy of the record.
func (r ControllerRecord) Clone() ControllerRecord {
	r.ValidationErrors = append([]string(nil), r.ValidationErrors...)
	return r
}
