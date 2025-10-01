module github.com/harishhary/blink

go 1.22.5

require github.com/cenkalti/backoff/v4 v4.3.0

require (
	github.com/aws/aws-lambda-go v1.47.0
	github.com/aws/aws-sdk-go-v2 v1.39.2
	github.com/aws/aws-sdk-go-v2/config v1.30.2
	github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue v1.20.13
	github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression v1.7.29
	github.com/aws/aws-sdk-go-v2/service/dynamodb v1.50.5
	github.com/aws/aws-sdk-go-v2/service/lambda v1.56.3
	github.com/google/uuid v1.6.0
)

require (
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.6.3 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.18.2 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.18.1 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.9 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.9 // indirect
	github.com/aws/aws-sdk-go-v2/internal/ini v1.8.3 // indirect
	github.com/aws/aws-sdk-go-v2/service/dynamodbstreams v1.31.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/endpoint-discovery v1.11.9 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.26.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.31.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.35.1 // indirect
	github.com/aws/smithy-go v1.23.0 // indirect
	github.com/jmespath/go-jmespath v0.4.0 // indirect
)
