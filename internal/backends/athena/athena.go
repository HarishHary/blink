package athena

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/athena"
	"github.com/aws/aws-sdk-go-v2/service/athena/types"
	"github.com/harishhary/blink/internal/backends"
	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/pkg/alerts"
	_ "github.com/mattn/go-sqlite3"
)

type AthenaBackend struct {
	ctx       context.Context
	athenaSvc *athena.Client
	dbName    string
	tableName string
}

func NewAthenaBackend(ctx context.Context, dbName, tableName string) (*AthenaBackend, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	return &AthenaBackend{
		ctx:       ctx,
		athenaSvc: athena.NewFromConfig(cfg),
		dbName:    dbName,
		tableName: tableName,
	}, nil
}

func (a *AthenaBackend) RuleNamesGenerator() <-chan string {
	out := make(chan string)
	go func() {
		defer close(out)
		query := fmt.Sprintf("SELECT DISTINCT RuleName FROM %s.%s", a.dbName, a.tableName)
		results, err := a.executeAthenaQuery(query)
		if err != nil {
			fmt.Printf("Error executing Athena query: %v\n", err)
			return
		}

		ruleNames := make(map[string]struct{})
		for _, row := range results {
			ruleName := row["RuleName"].(string)
			if _, exists := ruleNames[ruleName]; !exists {
				ruleNames[ruleName] = struct{}{}
				out <- ruleName
			}
		}
	}()
	return out
}

func (a *AthenaBackend) GetAlertRecords(ruleName string, alertProcTimeoutSec int) <-chan backends.Record {
	out := make(chan backends.Record)
	go func() {
		defer close(out)
		inProgressThreshold := time.Now().Add(-time.Duration(alertProcTimeoutSec) * time.Second).Format(helpers.DATETIME_FORMAT)
		query := fmt.Sprintf("SELECT * FROM %s.%s WHERE RuleName = '%s' AND Dispatched < '%s'", a.dbName, a.tableName, ruleName, inProgressThreshold)
		results, err := a.executeAthenaQuery(query)
		if err != nil {
			fmt.Printf("Error executing Athena query: %v\n", err)
			return
		}

		for _, row := range results {
			record := a.mapToRecord(row)
			out <- record
		}
	}()
	return out
}

func (a *AthenaBackend) GetAlertRecord(ruleName string, alertID string) (backends.Record, error) {
	query := fmt.Sprintf("SELECT * FROM %s.%s WHERE RuleName = '%s' AND AlertID = '%s'", a.dbName, a.tableName, ruleName, alertID)
	results, err := a.executeAthenaQuery(query)
	if err != nil {
		return nil, fmt.Errorf("error executing Athena query: %w", err)
	}

	if len(results) == 0 {
		return nil, nil
	}

	record := a.mapToRecord(results[0])
	return record, nil
}

func (a *AthenaBackend) AddAlerts(alerts []*alerts.Alert) error {
	for _, alert := range alerts {
		record, err := a.ToRecord(alert)
		if err != nil {
			return fmt.Errorf("error marshalling alert: %w", err)
		}

		// Construct the SQL INSERT statement
		columns := []string{}
		values := []string{}
		for k, v := range record {
			columns = append(columns, k)
			values = append(values, fmt.Sprintf("'%v'", v))
		}
		query := fmt.Sprintf("INSERT INTO %s.%s (%s) VALUES (%s)", a.dbName, a.tableName, strings.Join(columns, ","), strings.Join(values, ","))

		_, err = a.executeAthenaQuery(query)
		if err != nil {
			return fmt.Errorf("error executing Athena insert query: %w", err)
		}
	}

	return nil
}

func (a *AthenaBackend) DeleteAlerts(alerts []*alerts.Alert) error {
	for _, alert := range alerts {
		record, err := a.ToRecord(alert)
		if err != nil {
			return fmt.Errorf("error marshalling alert: %w", err)
		}

		// Construct the SQL DELETE statement
		query := fmt.Sprintf("DELETE FROM %s.%s WHERE RuleName = '%s' AND AlertID = '%s'", a.dbName, a.tableName, record["RuleName"], record["AlertID"])

		_, err = a.executeAthenaQuery(query)
		if err != nil {
			return fmt.Errorf("error executing Athena delete query: %w", err)
		}
	}

	return nil
}

func (a *AthenaBackend) UpdateSentOutputs(alert *alerts.Alert) error {
	recordKey := alert.RecordKey()
	update := fmt.Sprintf("OutputsSent = '%s'", strings.Join(alert.OutputsSent, ","))
	query := fmt.Sprintf("UPDATE %s.%s SET %s WHERE RuleName = '%s' AND AlertID = '%s'", a.dbName, a.tableName, update, recordKey["RuleName"], recordKey["AlertID"])

	_, err := a.executeAthenaQuery(query)
	if err != nil {
		return fmt.Errorf("error executing Athena update query: %w", err)
	}

	return nil
}

func (a *AthenaBackend) MarkAsDispatched(alert *alerts.Alert) error {
	recordKey := alert.RecordKey()
	update := fmt.Sprintf("Attempts = %d, Dispatched = '%s'", alert.Attempts, alert.Dispatched.Format(helpers.DATETIME_FORMAT))
	query := fmt.Sprintf("UPDATE %s.%s SET %s WHERE RuleName = '%s' AND AlertID = '%s'", a.dbName, a.tableName, update, recordKey["RuleName"], recordKey["AlertID"])

	_, err := a.executeAthenaQuery(query)
	if err != nil {
		return fmt.Errorf("error executing Athena update query: %w", err)
	}

	return nil
}

func (a *AthenaBackend) ToAlert(record backends.Record) (*alerts.Alert, error) {
	alert := new(alerts.Alert)

	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal record: %w", err)
	}
	if err := json.Unmarshal(data, alert); err != nil {
		return nil, fmt.Errorf("failed to unmarshal record to alert: %w", err)
	}

	if createdStr, ok := record["Created"].(string); ok {
		alert.Created, err = time.Parse(helpers.DATETIME_FORMAT, createdStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse Created timestamp: %w", err)
		}
	}

	if dispatchedStr, ok := record["Dispatched"].(string); ok {
		dispatchedTime, err := time.Parse(helpers.DATETIME_FORMAT, dispatchedStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse Dispatched timestamp: %w", err)
		}
		alert.Dispatched = dispatchedTime
	}

	if eventStr, ok := record["Event"].(string); ok {
		err = json.Unmarshal([]byte(eventStr), &alert.Event)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal Event JSON: %w", err)
		}
	}
	return alert, nil
}

func (a *AthenaBackend) ToRecord(alert *alerts.Alert) (backends.Record, error) {
	record := backends.Record{
		"RuleName":        alert.Rule.Name(),
		"AlertID":         alert.AlertID,
		"Attempts":        alert.Attempts,
		"Cluster":         alert.Cluster,
		"Created":         alert.Created.Format(helpers.DATETIME_FORMAT),
		"Dispatched":      alert.Dispatched.Format(helpers.DATETIME_FORMAT),
		"LogSource":       alert.LogSource,
		"LogType":         alert.LogType,
		"MergeByKeys":     alert.Rule.MergeByKeys(),
		"MergeWindowMins": alert.Rule.MergeWindowMins(),
		"Dispatchers":     alert.Rule.Dispatchers(),
		"OutputsSent":     alert.OutputsSent,
		"Formatters":      alert.Rule.Formatters(),
		"Event":           helpers.JsonCompact(alert.Event),
		"RuleDescription": alert.Rule.Description(),
		"SourceEntity":    alert.SourceEntity,
		"SourceService":   alert.SourceService,
		"Staged":          alert.Staged,
	}

	return record, nil
}

func (a *AthenaBackend) executeAthenaQuery(query string) ([]map[string]interface{}, error) {
	startQueryExecutionInput := &athena.StartQueryExecutionInput{
		QueryString: aws.String(query),
		QueryExecutionContext: &types.QueryExecutionContext{
			Database: aws.String(a.dbName),
		},
		ResultConfiguration: &types.ResultConfiguration{
			OutputLocation: aws.String("s3://your-athena-query-results-bucket/"),
		},
	}

	startQueryExecutionOutput, err := a.athenaSvc.StartQueryExecution(a.ctx, startQueryExecutionInput)
	if err != nil {
		return nil, fmt.Errorf("failed to start query execution: %w", err)
	}

	getQueryExecutionInput := &athena.GetQueryExecutionInput{
		QueryExecutionId: startQueryExecutionOutput.QueryExecutionId,
	}

	for {
		getQueryExecutionOutput, err := a.athenaSvc.GetQueryExecution(a.ctx, getQueryExecutionInput)
		if err != nil {
			return nil, fmt.Errorf("failed to get query execution: %w", err)
		}

		state := getQueryExecutionOutput.QueryExecution.Status.State
		if state == types.QueryExecutionStateSucceeded {
			break
		} else if state == types.QueryExecutionStateFailed || state == types.QueryExecutionStateCancelled {
			return nil, fmt.Errorf("query execution failed or was cancelled: %v", getQueryExecutionOutput.QueryExecution.Status.StateChangeReason)
		}

		time.Sleep(2 * time.Second)
	}

	getQueryResultsInput := &athena.GetQueryResultsInput{
		QueryExecutionId: startQueryExecutionOutput.QueryExecutionId,
	}

	getQueryResultsOutput, err := a.athenaSvc.GetQueryResults(a.ctx, getQueryResultsInput)
	if err != nil {
		return nil, fmt.Errorf("failed to get query results: %w", err)
	}

	var results []map[string]interface{}
	for _, row := range getQueryResultsOutput.ResultSet.Rows {
		result := make(map[string]interface{})
		for i, datum := range row.Data {
			result[*getQueryResultsOutput.ResultSet.ResultSetMetadata.ColumnInfo[i].Name] = *datum.VarCharValue
		}
		results = append(results, result)
	}

	return results, nil
}

func (a *AthenaBackend) mapToRecord(row map[string]interface{}) backends.Record {
	record := make(backends.Record)
	for k, v := range row {
		record[k] = v
	}
	return record
}
