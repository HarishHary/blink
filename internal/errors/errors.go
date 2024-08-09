package errors

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Error interface {
	WithContext(context ...any)
	Error() string
	Message() any
	Wrap(other Error)
	CorrelationID() string
}

type BaseError struct {
	message       any    `json:"-"`
	context       []any  `json:"-"`
	correlationID string `json:"-"`
	wrapped       Error  `json:"-"`
	file          string `json:"-"`
	line          int    `json:"-"`
}

func New(message any) *BaseError {
	_, file, line, ok := runtime.Caller(3)
	if !ok {
		file = "???"
		line = 0
	}

	uuid := uuid.NewString()
	return &BaseError{
		message:       message,
		context:       make([]any, 0),
		correlationID: uuid,
		file:          file,
		line:          line,
	}
}

func NewE(err error) Error {
	switch e := err.(type) {
	case Error:
		return e
	default:
		return New(err.Error())
	}
}

func NewF(format string, args ...any) *BaseError {
	return New(fmt.Sprintf(format, args...))
}

func (err *BaseError) WithContext(context ...any) {
	err.context = append(err.context, context...)
}

func (err *BaseError) Wrap(other Error) {
	err.correlationID = other.CorrelationID()
	err.wrapped = other
}

func (err *BaseError) CorrelationID() string {
	return err.correlationID
}

func (err *BaseError) UnmarshalJSON(data []byte) error {
	result := struct {
		Correlation string `json:"correlation"`
		Message     any    `json:"message"`
	}{}

	if err := json.Unmarshal(data, &result); err != nil {
		return err
	}

	*err = *New(result.Message)
	err.correlationID = result.Correlation
	return nil
}

func (err *BaseError) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{
		"correlation": err.correlationID,
		"message":     err.message,
	})
}

func (err *BaseError) GRPCStatus() *status.Status {
	data, _ := json.Marshal(err)
	return status.New(codes.Internal, string(data))
}

func (err *BaseError) Message() any {
	return err.message
}

func (err *BaseError) Error() string {
	builder := strings.Builder{}
	prefix := fmt.Sprintf("[%s:%d][%s]", err.file, err.line, err.correlationID)

	builder.WriteString(fmt.Sprintf("%s[Error] %s", prefix, err.message))
	for _, context := range err.context {
		builder.WriteString(fmt.Sprintf("\n%s[Context] %s", prefix, Print(context)))
	}

	if err.wrapped != nil {
		wrapped := err.wrapped.Error()
		builder.WriteString(fmt.Sprintf("\n%s", wrapped))
	}

	return builder.String()
}
