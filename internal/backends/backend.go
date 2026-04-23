package backends

import (
	"context"

	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/rules"
)

type Record map[string]any

// IAlertStore covers alert persistence: CRUD, serialisation, and streaming reads.
type IAlertStore interface {
	AddAlerts(alerts []*alerts.Alert) error
	DeleteAlerts(alerts []*alerts.Alert) error
	UpdateSentOutputs(alerts *alerts.Alert) error
	// GetAlertRecords streams alert records for ruleName dispatched within alertProcTimeoutSec.
	// ctx cancellation stops the streaming goroutine.
	GetAlertRecords(ctx context.Context, ruleName string, alertProcTimeoutSec int) <-chan Record
	GetAlertRecord(ruleName string, alertID string) (Record, error)
	MarkAsDispatched(alert *alerts.Alert) error
	ToAlert(record Record) (*alerts.Alert, error)
	ToRecord(alert *alerts.Alert) (Record, error)
}

// IRuleStore covers rule-level queries (distinct rule names + bulk rule fetch).
type IRuleStore interface {
	RuleNamesGenerator() <-chan string
	FetchAllRules() (<-chan *rules.RuleMetadata, error)
}

// IBackend is the full backend capability: alert store + rule store.
// Individual backends may implement only IAlertStore or IRuleStore as appropriate.
type IBackend interface {
	IAlertStore
	IRuleStore
}
