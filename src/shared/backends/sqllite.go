package backends

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/harishhary/blink/src/shared/alerts"
	"github.com/harishhary/blink/src/shared/helpers"
	_ "github.com/mattn/go-sqlite3"
)

type SQLiteBackend struct {
	ctx    context.Context
	db     *sql.DB
	dbName string
}

func NewSQLiteBackend(ctx context.Context, dbName string) (*SQLiteBackend, error) {
	db, err := sql.Open("sqlite3", dbName)
	if err != nil {
		return nil, fmt.Errorf("failed to open SQLite database: %w", err)
	}

	return &SQLiteBackend{
		ctx:    ctx,
		db:     db,
		dbName: dbName,
	}, nil
}

func scanRecord(rows *sql.Rows) (Record, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("failed to get columns: %w", err)
	}

	values := make([]interface{}, len(cols))
	valuePtrs := make([]interface{}, len(cols))
	for i := range values {
		valuePtrs[i] = &values[i]
	}

	if err := rows.Scan(valuePtrs...); err != nil {
		return nil, fmt.Errorf("failed to scan record: %w", err)
	}

	record := make(Record)
	for i, col := range cols {
		val := values[i]
		if b, ok := val.([]byte); ok {
			record[col] = string(b)
		} else {
			record[col] = val
		}
	}
	return record, nil
}

func scanSingleRecord(row *sql.Row) (Record, error) {
	var ruleName, alertID, attempts, cluster, created, dispatched, logSource, logType, mergeByKeys, mergeWindow, outputs, outputsSent, formatters, recordStr, ruleDescription, sourceEntity, sourceService, staged string

	err := row.Scan(
		&ruleName, &alertID, &attempts, &cluster, &created,
		&dispatched, &logSource, &logType, &mergeByKeys, &mergeWindow,
		&outputs, &outputsSent, &formatters, &recordStr, &ruleDescription,
		&sourceEntity, &sourceService, &staged,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to scan record: %w", err)
	}

	record := Record{
		"RuleName":        ruleName,
		"AlertID":         alertID,
		"Attempts":        attempts,
		"Cluster":         cluster,
		"Created":         created,
		"Dispatched":      dispatched,
		"LogSource":       logSource,
		"LogType":         logType,
		"MergeByKeys":     mergeByKeys,
		"MergeWindow":     mergeWindow,
		"Outputs":         outputs,
		"OutputsSent":     outputsSent,
		"Formatters":      formatters,
		"Record":          recordStr,
		"RuleDescription": ruleDescription,
		"SourceEntity":    sourceEntity,
		"SourceService":   sourceService,
		"Staged":          staged,
	}
	return record, nil
}

func (s *SQLiteBackend) RuleNamesGenerator() <-chan string {
	out := make(chan string)
	go func() {
		defer close(out)

		rows, err := s.db.QueryContext(s.ctx, "SELECT DISTINCT RuleName FROM alerts")
		if err != nil {
			fmt.Printf("Error querying rule names: %v\n", err)
			return
		}
		defer rows.Close()

		ruleNames := make(map[string]struct{})
		for rows.Next() {
			var ruleName string
			if err := rows.Scan(&ruleName); err != nil {
				fmt.Printf("Error scanning rule name: %v\n", err)
				return
			}
			if _, exists := ruleNames[ruleName]; !exists {
				ruleNames[ruleName] = struct{}{}
				out <- ruleName
			}
		}

		if err := rows.Err(); err != nil {
			fmt.Printf("Error iterating through rule names: %v\n", err)
		}
	}()
	return out
}

func (s *SQLiteBackend) GetAlertRecords(ruleName string, alertProcTimeoutSec int) <-chan Record {
	out := make(chan Record)
	go func() {
		defer close(out)

		inProgressThreshold := time.Now().Add(-time.Duration(alertProcTimeoutSec) * time.Second).Format(helpers.DATETIME_FORMAT)
		query := `SELECT * FROM alerts WHERE RuleName = ? AND Dispatched < ?`

		rows, err := s.db.QueryContext(s.ctx, query, ruleName, inProgressThreshold)
		if err != nil {
			fmt.Printf("Error querying alert records: %v\n", err)
			return
		}
		defer rows.Close()

		for rows.Next() {
			record, err := scanRecord(rows)
			if err != nil {
				fmt.Printf("Error scanning alert record: %v\n", err)
				return
			}
			out <- record
		}

		if err := rows.Err(); err != nil {
			fmt.Printf("Error iterating through alert records: %v\n", err)
		}
	}()
	return out
}

func (s *SQLiteBackend) GetAlertRecord(ruleName string, alertID string) (Record, error) {
	query := `SELECT * FROM alerts WHERE RuleName = ? AND AlertID = ?`
	row := s.db.QueryRowContext(s.ctx, query, ruleName, alertID)

	record, err := scanSingleRecord(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return record, err
}

func (s *SQLiteBackend) AddAlerts(alerts []*alerts.Alert) error {
	tx, err := s.db.BeginTx(s.ctx, nil)
	if err != nil {
		return fmt.Errorf("error beginning transaction: %w", err)
	}

	stmt, err := tx.PrepareContext(s.ctx, `
		INSERT INTO alerts (
			RuleName, AlertID, Attempts, Cluster, Created, Dispatched, LogSource, LogType,
			MergeByKeys, MergeWindow, Outputs, OutputsSent, Formatters, Record, RuleDescription,
			SourceEntity, SourceService, Staged
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("error preparing statement: %w", err)
	}
	defer stmt.Close()

	for _, alert := range alerts {
		record, err := s.ToRecord(alert)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("error marshalling alert: %w", err)
		}

		_, err = stmt.ExecContext(s.ctx, record["RuleName"], record["AlertID"], record["Attempts"], record["Cluster"],
			record["Created"], record["Dispatched"], record["LogSource"], record["LogType"], record["MergeByKeys"],
			record["MergeWindow"], record["Outputs"], record["OutputsSent"], record["Formatters"], record["Record"],
			record["RuleDescription"], record["SourceEntity"], record["SourceService"], record["Staged"])

		if err != nil {
			tx.Rollback()
			return fmt.Errorf("error executing insert: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("error committing transaction: %w", err)
	}

	return nil
}

func (s *SQLiteBackend) DeleteAlerts(alerts []*alerts.Alert) error {
	tx, err := s.db.BeginTx(s.ctx, nil)
	if err != nil {
		return fmt.Errorf("error beginning transaction: %w", err)
	}

	stmt, err := tx.PrepareContext(s.ctx, `DELETE FROM alerts WHERE RuleName = ? AND AlertID = ?`)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("error preparing statement: %w", err)
	}
	defer stmt.Close()

	for _, alert := range alerts {
		_, err := stmt.ExecContext(s.ctx, alert.RuleName, alert.AlertID)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("error executing delete: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("error committing transaction: %w", err)
	}

	return nil
}

func (s *SQLiteBackend) UpdateSentOutputs(alert *alerts.Alert) error {
	query := `UPDATE alerts SET OutputsSent = ? WHERE RuleName = ? AND AlertID = ?`
	_, err := s.db.ExecContext(s.ctx, query, alert.OutputsSent, alert.RuleName, alert.AlertID)
	if err != nil {
		return fmt.Errorf("error updating item: %w", err)
	}
	return nil
}

func (s *SQLiteBackend) MarkAsDispatched(alert *alerts.Alert) error {
	query := `UPDATE alerts SET Attempts = ?, Dispatched = ? WHERE RuleName = ? AND AlertID = ?`
	_, err := s.db.ExecContext(s.ctx, query, alert.Attempts, alert.Dispatched.Format(helpers.DATETIME_FORMAT), alert.RuleName, alert.AlertID)
	if err != nil {
		return fmt.Errorf("error updating item: %w", err)
	}
	return nil
}

func (s *SQLiteBackend) ToAlert(record Record) (*alerts.Alert, error) {
	a := new(alerts.Alert)

	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal record: %w", err)
	}
	if err := json.Unmarshal(data, a); err != nil {
		return nil, fmt.Errorf("failed to unmarshal record to alert: %w", err)
	}

	if createdStr, ok := record["Created"].(string); ok {
		a.Created, err = time.Parse(helpers.DATETIME_FORMAT, createdStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse Created timestamp: %w", err)
		}
	}

	if dispatchedStr, ok := record["Dispatched"].(string); ok {
		dispatchedTime, err := time.Parse(helpers.DATETIME_FORMAT, dispatchedStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse Dispatched timestamp: %w", err)
		}
		a.Dispatched = dispatchedTime
	}

	if recordStr, ok := record["Record"].(string); ok {
		err = json.Unmarshal([]byte(recordStr), &a.Record)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal Record JSON: %w", err)
		}
	}
	return a, nil
}

func (s *SQLiteBackend) ToRecord(alert *alerts.Alert) (Record, error) {
	record := Record{
		"RuleName":        alert.RuleName,
		"AlertID":         alert.AlertID,
		"Attempts":        alert.Attempts,
		"Cluster":         alert.Cluster,
		"Created":         alert.Created.Format(helpers.DATETIME_FORMAT),
		"Dispatched":      alert.Dispatched.Format(helpers.DATETIME_FORMAT),
		"LogSource":       alert.LogSource,
		"LogType":         alert.LogType,
		"MergeByKeys":     alert.MergeByKeys,
		"MergeWindow":     alert.MergeWindow,
		"Outputs":         alert.Dispatchers,
		"OutputsSent":     alert.OutputsSent,
		"Formatters":      alert.Formatters,
		"Record":          helpers.JsonCompact(alert.Record),
		"RuleDescription": alert.RuleDescription,
		"SourceEntity":    alert.SourceEntity,
		"SourceService":   alert.SourceService,
		"Staged":          alert.Staged,
	}

	return record, nil
}
