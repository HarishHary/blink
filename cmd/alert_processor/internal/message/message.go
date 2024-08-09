package message

import "github.com/harishhary/blink/internal/messaging"

const (
	SyncService       = messaging.ServiceName(iota)
	DispatcherService = messaging.ServiceName(iota)
	AlertService      = messaging.ServiceName(iota)
)
