package backends

import (
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/rules"
)

type Record map[string]any

type IBackend interface {
	AddAlerts(alerts []*alerts.Alert) error
	DeleteAlerts(alerts []*alerts.Alert) error
	UpdateSentOutputs(alerts *alerts.Alert) error
	GetAlertRecords(ruleName string, alertProcTimeoutSec int) <-chan Record
	GetAlertRecord(ruleName string, alertID string) (Record, error)
	RuleNamesGenerator() <-chan string
	MarkAsDispatched(alert *alerts.Alert) error
	ToAlert(record Record) (*alerts.Alert, error)
	ToRecord(alert *alerts.Alert) (Record, error)
	FetchAllRules() (<-chan rules.IRule, error)
}
