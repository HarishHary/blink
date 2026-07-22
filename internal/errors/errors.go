package errors

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// PluginErrorStatus classifies a plugin callback error for a batch RPC. Plain
// errors are record-scoped; context and explicit non-InvalidArgument statuses
// fail the RPC call. BaseError's synthetic Internal status is not explicit.
func PluginErrorStatus(err error) *status.Status {
	if err == nil {
		return status.New(codes.OK, "")
	}
	if stderrors.Is(err, context.Canceled) || stderrors.Is(err, context.DeadlineExceeded) {
		return status.FromContextError(err)
	}

	var record *status.Status
	var visit func(error) *status.Status
	visit = func(err error) *status.Status {
		if err == nil {
			return nil
		}
		if provider, ok := err.(interface{ GRPCStatus() *status.Status }); ok {
			if s := provider.GRPCStatus(); s != nil && s.Code() != codes.OK && !(isBaseError(err) && s.Code() == codes.Internal) {
				if s.Code() != codes.InvalidArgument {
					return s
				}
				record = s
			}
		}
		if wrapped, ok := err.(interface{ Unwrap() []error }); ok {
			for _, cause := range wrapped.Unwrap() {
				if s := visit(cause); s != nil {
					return s
				}
			}
		}
		if wrapped, ok := err.(interface{ Unwrap() error }); ok {
			return visit(wrapped.Unwrap())
		}
		return nil
	}
	if s := visit(err); s != nil {
		return s
	}
	if record != nil {
		return record
	}
	return status.New(codes.InvalidArgument, err.Error())
}

func isBaseError(err error) bool {
	_, ok := err.(*BaseError)
	return ok
}

type Error interface {
	WithContext(context ...any)
	Error() string
	Message() any
	Wrap(other Error)
	CorrelationID() string
}

// ResultCardinalityError reports a plugin batch response whose shape does not
// match its request.
type ResultCardinalityError struct {
	PluginKind string
	PluginID   string
	Field      string
	Expected   int
	Actual     int
}

func (err *ResultCardinalityError) Error() string {
	field := err.Field
	if field == "" {
		field = "results"
	}
	return fmt.Sprintf("%s %s returned %d %s for %d items", err.PluginKind, err.PluginID, err.Actual, field, err.Expected)
}

type BaseError struct {
	message       any    `json:"-"`
	context       []any  `json:"-"`
	correlationID string `json:"-"`
	wrapped       Error  `json:"-"`
	cause         error  `json:"-"`
	file          string `json:"-"`
	line          int    `json:"-"`
}

func newBase(message any, file string, line int) *BaseError {
	return &BaseError{
		message:       message,
		context:       make([]any, 0),
		correlationID: uuid.NewString(),
		file:          file,
		line:          line,
	}
}

func New(message any) *BaseError {
	_, file, line, ok := runtime.Caller(1)
	if !ok {
		file = "???"
	}
	return newBase(message, file, line)
}

func NewE(err error) Error {
	switch e := err.(type) {
	case Error:
		return e
	default:
		_, file, line, ok := runtime.Caller(1)
		if !ok {
			file = "???"
		}
		base := newBase(err.Error(), file, line)
		base.cause = err
		return base
	}
}

func NewF(format string, args ...any) *BaseError {
	_, file, line, ok := runtime.Caller(1)
	if !ok {
		file = "???"
	}
	return newBase(fmt.Sprintf(format, args...), file, line)
}

func (err *BaseError) WithContext(context ...any) {
	err.context = append(err.context, context...)
}

func (err *BaseError) Wrap(other Error) {
	err.correlationID = other.CorrelationID()
	err.wrapped = other
}

// Unwrap returns the displayed wrapped error followed by the original cause.
func (err *BaseError) Unwrap() []error {
	var unwrapped []error
	if err.wrapped != nil {
		unwrapped = append(unwrapped, err.wrapped)
	}
	if err.cause != nil {
		unwrapped = append(unwrapped, err.cause)
	}
	return unwrapped
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
	if err.cause != nil {
		if cause, ok := status.FromError(err.cause); ok {
			return cause
		}
	}
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
