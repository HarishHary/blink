package dynamodb

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/harishhary/blink/internal/backends"
	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/rules"
)

type DynamoDBBackend struct {
	ctx    context.Context
	db     *dynamodb.Client
	dbName string
}

func NewDynamoDBBackend(dbName string) (*DynamoDBBackend, error) {
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	return &DynamoDBBackend{
		ctx:    ctx,
		db:     dynamodb.NewFromConfig(cfg),
		dbName: dbName,
	}, nil
}

// PaginateScan function for DynamoDB Query
func (at *DynamoDBBackend) paginateScan(scanFunc func(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error), input *dynamodb.ScanInput) <-chan map[string]types.AttributeValue {
	out := make(chan map[string]types.AttributeValue)
	go func() {
		defer close(out)
		for {
			result, err := scanFunc(at.ctx, input)
			if err != nil {
				fmt.Printf("Error paginating scan: %v", err)
				return
			}

			for _, item := range result.Items {
				out <- item
			}

			if result.LastEvaluatedKey == nil {
				return
			}
			input.ExclusiveStartKey = result.LastEvaluatedKey
		}
	}()
	return out
}

// PaginateQuery function for DynamoDB Query
func (at *DynamoDBBackend) paginateQuery(queryFunc func(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error), input *dynamodb.QueryInput) <-chan map[string]types.AttributeValue {
	out := make(chan map[string]types.AttributeValue)
	go func() {
		defer close(out)
		for {
			result, err := queryFunc(at.ctx, input)
			if err != nil {
				fmt.Printf("Error paginating query: %v", err)
				return
			}

			for _, item := range result.Items {
				out <- item
			}

			if result.LastEvaluatedKey == nil {
				return
			}
			input.ExclusiveStartKey = result.LastEvaluatedKey
		}
	}()
	return out
}

func (at *DynamoDBBackend) RuleNamesGenerator() <-chan string {
	input := &dynamodb.ScanInput{
		TableName:            aws.String(at.dbName),
		ProjectionExpression: aws.String("RuleName"),
		Select:               types.SelectSpecificAttributes,
		ConsistentRead:       aws.Bool(false),
	}

	ruleNames := make(map[string]struct{})
	out := make(chan string)

	go func() {
		defer close(out)
		generator := at.paginateScan(at.db.Scan, input)
		for item := range generator {
			ruleName := *item["RuleName"].(*types.AttributeValueMemberS)
			if _, exists := ruleNames[ruleName.Value]; !exists {
				ruleNames[ruleName.Value] = struct{}{}
				out <- ruleName.Value
			}
		}
	}()
	return out
}

func (at *DynamoDBBackend) GetAlertRecords(ruleName string, alertProcTimeoutSec int) <-chan backends.Record {
	inProgressThreshold := time.Now().Add(-time.Duration(alertProcTimeoutSec) * time.Second).Format(helpers.DATETIME_FORMAT)
	filter := expression.Name("Dispatched").LessThan(expression.Value(inProgressThreshold))
	keyCond := expression.Key("RuleName").Equal(expression.Value(ruleName))

	expr, err := expression.NewBuilder().WithFilter(filter).WithKeyCondition(keyCond).Build()
	if err != nil {
		fmt.Printf("Error building expression: %v", err)
		return nil
	}

	input := &dynamodb.QueryInput{
		TableName:                 aws.String(at.dbName),
		ConsistentRead:            aws.Bool(true),
		ExpressionAttributeNames:  expr.Names(),
		ExpressionAttributeValues: expr.Values(),
		FilterExpression:          expr.Filter(),
		KeyConditionExpression:    expr.KeyCondition(),
	}
	out := make(chan backends.Record)
	go func() {
		defer close(out)
		for item := range at.paginateQuery(at.db.Query, input) {
			var value backends.Record
			_ = attributevalue.UnmarshalMap(item, &value)
			out <- value
		}
	}()
	return out
}

func (at *DynamoDBBackend) GetAlertRecord(ruleName string, alertID string) (backends.Record, error) {
	keyCond := expression.Key("RuleName").Equal(expression.Value(ruleName)).And(expression.Key("AlertID").Equal(expression.Value(alertID)))

	expr, err := expression.NewBuilder().WithKeyCondition(keyCond).Build()
	if err != nil {
		return nil, fmt.Errorf("error building expression: %w", err)
	}

	input := &dynamodb.QueryInput{
		TableName:                 aws.String(at.dbName),
		ConsistentRead:            aws.Bool(true),
		ExpressionAttributeNames:  expr.Names(),
		ExpressionAttributeValues: expr.Values(),
		KeyConditionExpression:    expr.KeyCondition(),
	}

	result, err := at.db.Query(at.ctx, input)
	if err != nil {
		return nil, fmt.Errorf("error querying table: %w", err)
	}

	if len(result.Items) == 0 {
		return nil, nil
	}

	var value backends.Record
	_ = attributevalue.UnmarshalMap(result.Items[0], &value)

	return value, nil
}

func (at *DynamoDBBackend) AddAlerts(alerts []*alerts.Alert) error {
	batchWrite := &dynamodb.BatchWriteItemInput{}
	for _, alert := range alerts {
		dynamoRecord, err := at.ToRecord(alert)
		if err != nil {
			return fmt.Errorf("error marshalling alert: %w", err)
		}
		item, err := attributevalue.MarshalMap(dynamoRecord)
		if err != nil {
			return fmt.Errorf("error marshalling alert: %w", err)
		}

		batchWrite.RequestItems[at.dbName] = append(batchWrite.RequestItems[at.dbName], types.WriteRequest{
			PutRequest: &types.PutRequest{
				Item: item,
			},
		})
	}

	_, err := at.db.BatchWriteItem(at.ctx, batchWrite)
	if err != nil {
		return fmt.Errorf("error writing batch: %w", err)
	}

	return nil
}

func (at *DynamoDBBackend) DeleteAlerts(alerts []*alerts.Alert) error {
	batchWrite := &dynamodb.BatchWriteItemInput{}
	for _, alert := range alerts {
		key := alert.RecordKey()
		item := map[string]types.AttributeValue{
			"RuleName": &types.AttributeValueMemberS{Value: *aws.String(key["RuleName"])},
			"AlertID":  &types.AttributeValueMemberS{Value: *aws.String(key["AlertID"])},
		}
		batchWrite.RequestItems[at.dbName] = append(batchWrite.RequestItems[at.dbName], types.WriteRequest{
			DeleteRequest: &types.DeleteRequest{
				Key: item,
			},
		})
	}
	_, err := at.db.BatchWriteItem(at.ctx, batchWrite)
	if err != nil {
		return fmt.Errorf("error writing batch: %w", err)
	}
	return nil
}

func (at *DynamoDBBackend) UpdateSentOutputs(alert *alerts.Alert) error {
	record_key := alert.RecordKey()
	key, err := attributevalue.MarshalMap(record_key)
	if err != nil {
		return fmt.Errorf("error marshalling key: %w", err)
	}

	update := expression.Set(expression.Name("OutputsSent"), expression.Value(alert.OutputsSent))

	expr, err := expression.NewBuilder().WithUpdate(update).WithCondition(expression.AttributeExists(expression.Name("AlertID"))).Build()
	if err != nil {
		return fmt.Errorf("error building expression: %w", err)
	}

	input := &dynamodb.UpdateItemInput{
		TableName:                 aws.String(at.dbName),
		Key:                       key,
		ExpressionAttributeNames:  expr.Names(),
		ExpressionAttributeValues: expr.Values(),
		UpdateExpression:          expr.Update(),
		ConditionExpression:       expr.Condition(),
	}

	_, err = at.db.UpdateItem(at.ctx, input)
	if err != nil {
		return fmt.Errorf("error updating item: %w", err)
	}

	return nil
}

func (at *DynamoDBBackend) MarkAsDispatched(alert *alerts.Alert) error {
	dynamo_key := alert.RecordKey()
	key, err := attributevalue.MarshalMap(dynamo_key)
	if err != nil {
		return fmt.Errorf("error marshalling key: %w", err)
	}

	update := expression.Set(
		expression.Name("Attempts"), expression.Value(alert.Attempts)).
		Set(expression.Name("Dispatched"), expression.Value(alert.Dispatched.Format(helpers.DATETIME_FORMAT)))

	expr, err := expression.NewBuilder().WithUpdate(update).WithCondition(expression.AttributeExists(expression.Name("AlertID"))).Build()
	if err != nil {
		return fmt.Errorf("error building expression: %w", err)
	}

	input := &dynamodb.UpdateItemInput{
		TableName:                 aws.String(at.dbName),
		Key:                       key,
		ExpressionAttributeNames:  expr.Names(),
		ExpressionAttributeValues: expr.Values(),
		UpdateExpression:          expr.Update(),
		ConditionExpression:       expr.Condition(),
	}

	_, err = at.db.UpdateItem(at.ctx, input)
	if err != nil {
		return fmt.Errorf("error updating item: %w", err)
	}

	return nil
}

// CreateFromDynamoRecord creates an alert from a DynamoDB record
func (at *DynamoDBBackend) ToAlert(record backends.Record) (*alerts.Alert, error) {
	a := new(alerts.Alert)

	var dynamo_record, err = attributevalue.MarshalMap(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal record to dynamodb record: %w", err)
	}
	err = attributevalue.UnmarshalMap(dynamo_record, a)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal dynamodb record to alert: %w", err)
	}

	if createdStr, ok := record["Created"].(*types.AttributeValueMemberS); ok {
		a.Created, err = time.Parse(helpers.DATETIME_FORMAT, createdStr.Value)
		if err != nil {
			return nil, fmt.Errorf("failed to parse Created timestamp: %w", err)
		}
	}

	if dispatchedStr, ok := record["Dispatched"].(*types.AttributeValueMemberS); ok {
		dispatchedTime, err := time.Parse(helpers.DATETIME_FORMAT, dispatchedStr.Value)
		if err != nil {
			return nil, fmt.Errorf("failed to parse Dispatched timestamp: %w", err)
		}
		a.Dispatched = dispatchedTime
	}

	if eventStr, ok := record["Event"].(*types.AttributeValueMemberS); ok {
		err = json.Unmarshal([]byte(eventStr.Value), &a.Event)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal Event JSON: %w", err)
		}
	}
	return a, nil
}

func (at *DynamoDBBackend) ToRecord(alert *alerts.Alert) (backends.Record, error) {
	item, err := attributevalue.MarshalMap(backends.Record{
		"RuleName":        alert.Rule.Name(), // Partition Key
		"AlertID":         alert.AlertID,     // Sort/Range Key
		"Attempts":        alert.Attempts,
		"Cluster":         alert.Cluster,
		"Created":         alert.Created.Format(helpers.DATETIME_FORMAT),
		"Dispatched":      alert.Dispatched.Format(helpers.DATETIME_FORMAT),
		"LogSource":       alert.LogSource,
		"LogType":         alert.LogType,
		"MergeByKeys":     alert.Rule.MergeByKeys(),
		"MergeWindow":     alert.Rule.MergeWindowMins(),
		"Duspatchers":     alert.Rule.Dispatchers(),
		"OutputsSent":     alert.OutputsSent,
		"Formatters":      alert.Rule.Formatters(),
		"Event":           helpers.JsonCompact(alert.Event),
		"RuleDescription": alert.Rule.Description(),
		"SourceEntity":    alert.SourceEntity,
		"SourceService":   alert.SourceService,
		"Staged":          alert.Staged,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal alert to dynamodb record: %w", err)
	}
	var result backends.Record
	err = attributevalue.UnmarshalMap(item, result)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal alert to dynamodb record: %w", err)
	}
	return result, nil
}

func (at *DynamoDBBackend) FetchAllRules() (<-chan rules.IRule, error) {
	input := &dynamodb.ScanInput{
		TableName: aws.String(at.dbName),
		Select:    types.SelectAllAttributes,
	}

	out := make(chan rules.IRule)
	go func() {
		defer close(out)
		generator := at.paginateScan(at.db.Scan, input)
		for item := range generator {
			rule, err := at.unmarshalRule(item)
			if err != nil {
				fmt.Printf("failed to unmarshal rule: %v\n", err)
				continue
			}
			out <- rule
		}
	}()

	return out, nil
}

func (at *DynamoDBBackend) unmarshalRule(item map[string]types.AttributeValue) (rules.IRule, error) {
	var rule rules.IRule
	err := attributevalue.UnmarshalMap(item, &rule)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal item to rule: %w", err)
	}
	return rule, nil
}
