package backends

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
	"github.com/harishhary/blink/src/shared/alerts"
	"github.com/harishhary/blink/src/shared/helpers"
)

type DynamoDBBackend struct {
	ctx       context.Context
	svc       *dynamodb.Client
	tablename string
}

func NewDynamoDBBackend(ctx context.Context, tableName string) (*DynamoDBBackend, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	return &DynamoDBBackend{
		ctx:       ctx,
		svc:       dynamodb.NewFromConfig(cfg),
		tablename: tableName,
	}, nil
}

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
		TableName:            aws.String(at.tablename),
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

func (at *DynamoDBBackend) GetAlertRecords(ruleName string, alertProcTimeoutSec int) <-chan map[string]any {
	inProgressThreshold := time.Now().Add(-time.Duration(alertProcTimeoutSec) * time.Second).Format(helpers.DATETIME_FORMAT)

	filter := expression.Name("Dispatched").LessThan(expression.Value(inProgressThreshold))
	keyCond := expression.Key("RuleName").Equal(expression.Value(ruleName))

	expr, err := expression.NewBuilder().WithFilter(filter).WithKeyCondition(keyCond).Build()
	if err != nil {
		fmt.Printf("Error building expression: %v", err)
		return nil
	}

	input := &dynamodb.QueryInput{
		TableName:                 aws.String(at.tablename),
		ConsistentRead:            aws.Bool(true),
		ExpressionAttributeNames:  expr.Names(),
		ExpressionAttributeValues: expr.Values(),
		FilterExpression:          expr.Filter(),
		KeyConditionExpression:    expr.KeyCondition(),
	}
	out := make(chan map[string]any)
	go func() {
		defer close(out)
		for item := range at.paginateQuery(at.svc.Query, input) {
			var value map[string]any
			_ = attributevalue.UnmarshalMap(item, &value)
			out <- value
		}
	}()
	return out
}

func (at *DynamoDBBackend) GetAlertRecord(ruleName string, alertID string) (map[string]any, error) {
	keyCond := expression.Key("RuleName").Equal(expression.Value(ruleName)).And(expression.Key("AlertID").Equal(expression.Value(alertID)))

	expr, err := expression.NewBuilder().WithKeyCondition(keyCond).Build()
	if err != nil {
		return nil, fmt.Errorf("error building expression: %w", err)
	}

	input := &dynamodb.QueryInput{
		TableName:                 aws.String(at.tablename),
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

	var value map[string]any
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

		batchWrite.RequestItems[at.tablename] = append(batchWrite.RequestItems[at.tablename], types.WriteRequest{
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
		TableName:                 aws.String(at.tablename),
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

func (at *DynamoDBBackend) UpdateSentOutputs(alert *alerts.Alert) error {
	dynamo_key := alert.RecordKey()
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
		TableName:                 aws.String(at.tablename),
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

func (at *DynamoDBBackend) DeleteAlerts(keys [][]string) error {
	batchWrite := &dynamodb.BatchWriteItemInput{}
	for _, key := range keys {
		item := map[string]types.AttributeValue{
			"RuleName": &types.AttributeValueMemberS{Value: *aws.String(key[0])},
			"AlertID":  &types.AttributeValueMemberS{Value: *aws.String(key[1])},
		}

		batchWrite.RequestItems[at.tablename] = append(batchWrite.RequestItems[at.tablename], types.WriteRequest{
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

// CreateFromDynamoRecord creates an alert from a DynamoDB record
func (at *DynamoDBBackend) ToAlert(table_record map[string]any) (*alerts.Alert, error) {
	a := new(alerts.Alert)

	var dynamo_record, err = attributevalue.MarshalMap(table_record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal record to dynamodb record: %w", err)
	}
	err = attributevalue.UnmarshalMap(dynamo_record, a)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal dynamodb record to alert: %w", err)
	}

	if createdStr, ok := table_record["Created"].(*types.AttributeValueMemberS); ok {
		a.Created, err = time.Parse(helpers.DATETIME_FORMAT, createdStr.Value)
		if err != nil {
			return nil, fmt.Errorf("failed to parse Created timestamp: %w", err)
		}
	}

	if dispatchedStr, ok := table_record["Dispatched"].(*types.AttributeValueMemberS); ok {
		dispatchedTime, err := time.Parse(helpers.DATETIME_FORMAT, dispatchedStr.Value)
		if err != nil {
			return nil, fmt.Errorf("failed to parse Dispatched timestamp: %w", err)
		}
		a.Dispatched = dispatchedTime
	}

	if recordStr, ok := table_record["Record"].(*types.AttributeValueMemberS); ok {
		err = json.Unmarshal([]byte(recordStr.Value), &a.Record)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal Record JSON: %w", err)
		}
	}
	return a, nil
}

func (at *DynamoDBBackend) ToRecord(alert *alerts.Alert) (map[string]any, error) {
	item, err := attributevalue.MarshalMap(map[string]any{
		"RuleName":        alert.RuleName, // Partition Key
		"AlertID":         alert.AlertID,  // Sort/Range Key
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
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal alert to dynamodb record: %w", err)
	}
	var result map[string]any
	err = attributevalue.UnmarshalMap(item, result)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal alert to dynamodb record: %w", err)
	}
	return result, nil
}

// // Please this is some hacky shit to pass typing... Fucking help me to be a better software eng...
// // Converts MapAttributeValue to MapAny type...
// func convertMapAttributeValueToMapAny(attributeValueMap map[string]types.AttributeValue) map[string]any {
// 	result := make(map[string]any)

// 	for key, value := range attributeValueMap {
// 		result[key] = convertAttributeValueToAny(value)
// 	}
// 	return result
// }

// // Converts AttributeValue to Any type...
// func convertAttributeValueToAny(attrValue types.AttributeValue) any {
// 	switch v := attrValue.(type) {
// 	case *types.AttributeValueMemberS:
// 		return v.Value
// 	case *types.AttributeValueMemberN:
// 		return v.Value
// 	case *types.AttributeValueMemberB:
// 		return v.Value
// 	case *types.AttributeValueMemberSS:
// 		return v.Value
// 	case *types.AttributeValueMemberNS:
// 		return v.Value
// 	case *types.AttributeValueMemberBS:
// 		return v.Value
// 	case *types.AttributeValueMemberBOOL:
// 		return v.Value
// 	case *types.AttributeValueMemberNULL:
// 		return nil
// 	case *types.AttributeValueMemberM:
// 		return convertMapAttributeValueToMapAny(v.Value)
// 	case *types.AttributeValueMemberL:
// 		list := make([]any, len(v.Value))
// 		for i, item := range v.Value {
// 			list[i] = convertAttributeValueToAny(item)
// 		}
// 		return list
// 	default:
// 		return v
// 	}
// }

// // Converts MapAny type to MapAttributeValue...
// func convertMapAnyToMapAttributeValue(anyMap map[string]any) map[string]types.AttributeValue {
// 	attrMap := make(map[string]types.AttributeValue)

// 	for key, value := range anyMap {
// 		attrMap[key] = convertAnyToAttributeValue(value)
// 	}

// 	return attrMap
// }

// // Converts Any type to AttributeValue...
// func convertAnyToAttributeValue(value any) types.AttributeValue {
// 	switch v := value.(type) {
// 	case string:
// 		return &types.AttributeValueMemberS{Value: v}
// 	case bool:
// 		return &types.AttributeValueMemberBOOL{Value: v}
// 	case int, int8, int16, int32, int64:
// 		return &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", v)}
// 	case uint, uint8, uint16, uint32, uint64:
// 		return &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", v)}
// 	case float32, float64:
// 		return &types.AttributeValueMemberN{Value: fmt.Sprintf("%f", v)}
// 	case []byte:
// 		return &types.AttributeValueMemberB{Value: v}
// 	case []string:
// 		return &types.AttributeValueMemberSS{Value: v}
// 	case []int, []int8, []int16, []int32, []int64, []uint, []uint16, []uint32, []uint64, []float32, []float64:
// 		var strValues []string
// 		val := reflect.ValueOf(v)
// 		for i := 0; i < val.Len(); i++ {
// 			strValues = append(strValues, fmt.Sprintf("%v", val.Index(i)))
// 		}
// 		return &types.AttributeValueMemberNS{Value: strValues}
// 	case []any:
// 		var attrValues []types.AttributeValue
// 		for _, item := range v {
// 			attrValues = append(attrValues, convertAnyToAttributeValue(item))
// 		}
// 		return &types.AttributeValueMemberL{Value: attrValues}
// 	case map[string]any:
// 		return &types.AttributeValueMemberM{Value: convertMapAnyToMapAttributeValue(v)}
// 	default:
// 		return &types.AttributeValueMemberNULL{Value: true}
// 	}
// }
