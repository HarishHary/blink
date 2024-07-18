package backends

import (
	"github.com/harishhary/blink/src/shared/alerts"
)

type IBackend interface {
	AddAlerts(alerts []*alerts.Alert) error
	DeleteAlerts(keys [][]string) error
	GetAlertRecords(ruleName string, alertProcTimeoutSec int) <-chan map[string]any
	GetAlertRecord(ruleName string, alertID string) (map[string]any, error)
	RuleNamesGenerator() <-chan string
	MarkAsDispatched(alert *alerts.Alert) error
	ToAlert(table_record map[string]any) (*alerts.Alert, error)
	ToRecord(alert *alerts.Alert) (map[string]any, error)
}
