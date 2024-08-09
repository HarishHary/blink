package message

import "github.com/harishhary/blink/internal/messaging"

const (
	SyncService  = messaging.ServiceName(iota)
	ExecService  = messaging.ServiceName(iota)
	AlertService = messaging.ServiceName(iota)
	GRPCService  = messaging.ServiceName(iota)
	HTTPService  = messaging.ServiceName(iota)
	EventService = messaging.ServiceName(iota)
)
