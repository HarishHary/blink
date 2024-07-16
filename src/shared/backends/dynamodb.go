package backends

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type DynamoDBReader struct {
	TableName string
	Client    *dynamodb.Client
}

func NewDynamoDBReader(tableName string) (*DynamoDBReader, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion("us-west-2"))
	if err != nil {
		return nil, err
	}
	return &DynamoDBReader{
		TableName: tableName,
		Client:    dynamodb.NewFromConfig(cfg),
	}, nil
}

func (r *DynamoDBReader) ReadData() ([]map[string]interface{}, error) {
	result, err := r.Client.Scan(context.TODO(), &dynamodb.ScanInput{
		TableName: aws.String(r.TableName),
	})
	if err != nil {
		return nil, err
	}

	var items []map[string]interface{}
	for _, i := range result.Items {
		item := map[string]interface{}{}
		for k, v := range i {
			item[k] = v.(*types.AttributeValueMemberS).Value
		}
		items = append(items, item)
	}

	return items, nil
}
