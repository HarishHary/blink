package alerts

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const datetimeFormat = "2006-01-02T15:04:05.000Z"

type AlertTable struct {
	ctx  context.Context
	svc  *dynamodb.Client
	name string
}

func NewAlertTable(ctx context.Context, tableName string) (*AlertTable, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	return &AlertTable{
		ctx:  ctx,
		svc:  dynamodb.NewFromConfig(cfg),
		name: tableName,
	}, nil
}

func (at *AlertTable) Name() string {
	return at.name
}

func (at *AlertTable) paginateScan(scanFunc func(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error), input *dynamodb.ScanInput) <-chan map[string]types.AttributeValue {
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
func (at *AlertTable) paginateQuery(queryFunc func(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error), input *dynamodb.QueryInput) <-chan map[string]types.AttributeValue {
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

func (at *AlertTable) RuleNamesGenerator() <-chan string {
	input := &dynamodb.ScanInput{
		TableName:            aws.String(at.name),
		ProjectionExpression: aws.String("RuleName"),
		Select:               types.SelectSpecificAttributes,
		ConsistentRead:       aws.Bool(false),
	}

	ruleNames := make(map[string]struct{})
	out := make(chan string)

	go func() {
		defer close(out)
		for item := range at.paginateScan(at.svc.Scan, input) {
			ruleName := *item["RuleName"].(*types.AttributeValueMemberS)
			if _, exists := ruleNames[ruleName.Value]; !exists {
				ruleNames[ruleName.Value] = struct{}{}
				out <- ruleName.Value
			}
		}
	}()
	return out
}

func (at *AlertTable) GetAlertRecords(ruleName string, alertProcTimeoutSec int) <-chan map[string]types.AttributeValue {
	inProgressThreshold := time.Now().Add(-time.Duration(alertProcTimeoutSec) * time.Second).Format(datetimeFormat)

	filter := expression.Name("Dispatched").LessThan(expression.Value(inProgressThreshold))
	keyCond := expression.Key("RuleName").Equal(expression.Value(ruleName))

	expr, err := expression.NewBuilder().WithFilter(filter).WithKeyCondition(keyCond).Build()
	if err != nil {
		fmt.Printf("Error building expression: %v", err)
		return nil
	}

	input := &dynamodb.QueryInput{
		TableName:                 aws.String(at.name),
		ConsistentRead:            aws.Bool(true),
		ExpressionAttributeNames:  expr.Names(),
		ExpressionAttributeValues: expr.Values(),
		FilterExpression:          expr.Filter(),
		KeyConditionExpression:    expr.KeyCondition(),
	}
	out := make(chan map[string]types.AttributeValue)
	go func() {
		defer close(out)
		for item := range at.paginateQuery(at.svc.Query, input) {
			out <- item
		}
	}()
	return out
}

func (at *AlertTable) GetAlertRecord(ruleName, alertID string) (map[string]types.AttributeValue, error) {
	keyCond := expression.Key("RuleName").Equal(expression.Value(ruleName)).And(expression.Key("AlertID").Equal(expression.Value(alertID)))

	expr, err := expression.NewBuilder().WithKeyCondition(keyCond).Build()
	if err != nil {
		return nil, fmt.Errorf("error building expression: %w", err)
	}

	input := &dynamodb.QueryInput{
		TableName:                 aws.String(at.name),
		ConsistentRead:            aws.Bool(true),
		ExpressionAttributeNames:  expr.Names(),
		ExpressionAttributeValues: expr.Values(),
		KeyConditionExpression:    expr.KeyCondition(),
	}

	result, err := at.svc.Query(at.ctx, input)
	if err != nil {
		return nil, fmt.Errorf("error querying table: %w", err)
	}

	if len(result.Items) == 0 {
		return nil, nil
	}

	return result.Items[0], nil
}

func (at *AlertTable) AddAlerts(alerts []*Alert) error {
	batchWrite := &dynamodb.BatchWriteItemInput{}
	for _, alert := range alerts {
		dynamo_record, err := alert.DynamoRecord()
		if err != nil {
			return fmt.Errorf("error marshalling alert: %w", err)
		}
		item, err := attributevalue.MarshalMap(dynamo_record)
		if err != nil {
			return fmt.Errorf("error marshalling alert: %w", err)
		}

		batchWrite.RequestItems[at.name] = append(batchWrite.RequestItems[at.name], types.WriteRequest{
			PutRequest: &types.PutRequest{
				Item: item,
			},
		})
	}

	_, err := at.svc.BatchWriteItem(at.ctx, batchWrite)
	if err != nil {
		return fmt.Errorf("error writing batch: %w", err)
	}

	return nil
}

func (at *AlertTable) MarkAsDispatched(alert *Alert) error {
	dynamo_key, err := alert.DynamoKey()
	if err != nil {
		return fmt.Errorf("error getting dynamo key: %w", err)
	}
	key, err := attributevalue.MarshalMap(dynamo_key)
	if err != nil {
		return fmt.Errorf("error marshalling key: %w", err)
	}

	update := expression.Set(
		expression.Name("Attempts"), expression.Value(alert.Attempts)).
		Set(expression.Name("Dispatched"), expression.Value(alert.Dispatched.Format(datetimeFormat)))

	expr, err := expression.NewBuilder().WithUpdate(update).WithCondition(expression.AttributeExists(expression.Name("AlertID"))).Build()
	if err != nil {
		return fmt.Errorf("error building expression: %w", err)
	}

	input := &dynamodb.UpdateItemInput{
		TableName:                 aws.String(at.name),
		Key:                       key,
		ExpressionAttributeNames:  expr.Names(),
		ExpressionAttributeValues: expr.Values(),
		UpdateExpression:          expr.Update(),
		ConditionExpression:       expr.Condition(),
	}

	_, err = at.svc.UpdateItem(at.ctx, input)
	if err != nil {
		return fmt.Errorf("error updating item: %w", err)
	}

	return nil
}

func (at *AlertTable) UpdateSentOutputs(alert *Alert) error {
	dynamo_key, err := alert.DynamoKey()
	if err != nil {
		return fmt.Errorf("error getting dynamo key: %w", err)
	}
	key, err := attributevalue.MarshalMap(dynamo_key)
	if err != nil {
		return fmt.Errorf("error marshalling key: %w", err)
	}

	update := expression.Set(expression.Name("OutputsSent"), expression.Value(alert.OutputsSent))

	expr, err := expression.NewBuilder().WithUpdate(update).WithCondition(expression.AttributeExists(expression.Name("AlertID"))).Build()
	if err != nil {
		return fmt.Errorf("error building expression: %w", err)
	}

	input := &dynamodb.UpdateItemInput{
		TableName:                 aws.String(at.name),
		Key:                       key,
		ExpressionAttributeNames:  expr.Names(),
		ExpressionAttributeValues: expr.Values(),
		UpdateExpression:          expr.Update(),
		ConditionExpression:       expr.Condition(),
	}

	_, err = at.svc.UpdateItem(at.ctx, input)
	if err != nil {
		return fmt.Errorf("error updating item: %w", err)
	}

	return nil
}

func (at *AlertTable) DeleteAlerts(keys [][]string) error {
	batchWrite := &dynamodb.BatchWriteItemInput{}
	for _, key := range keys {
		item := map[string]types.AttributeValue{
			"RuleName": &types.AttributeValueMemberS{Value: *aws.String(key[0])},
			"AlertID":  &types.AttributeValueMemberS{Value: *aws.String(key[1])},
		}

		batchWrite.RequestItems[at.name] = append(batchWrite.RequestItems[at.name], types.WriteRequest{
			DeleteRequest: &types.DeleteRequest{
				Key: item,
			},
		})
	}

	_, err := at.svc.BatchWriteItem(at.ctx, batchWrite)
	if err != nil {
		return fmt.Errorf("error writing batch: %w", err)
	}

	return nil
}
