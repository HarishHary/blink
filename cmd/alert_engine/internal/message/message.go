package message

import "github.com/harishhary/blink/internal/messaging"

const (
	SyncService     = messaging.ServiceName(iota)
	EnricherService = messaging.ServiceName(iota)
	TunerService    = messaging.ServiceName(iota)
	AlertService    = messaging.ServiceName(iota)
)
