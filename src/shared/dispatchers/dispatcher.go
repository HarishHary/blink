package dispatchers

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/harishhary/blink/src/shared/alerts"
	"github.com/harishhary/blink/src/shared/helpers"
)

// Logger initialized for package-wide use
var logger = log.Default()

// Constants for retry attempts and request timeout
const (
	MaxRetryAttempts      = 5
	DefaultRequestTimeout = 3*time.Second + 50*time.Millisecond // Close to 3.05
	DefaultServiceURL     = "https://example.com/api"           // Replace with actual URL
)

// DispatcherError custom error for Dispather
type DispatcherError struct {
	Message string
}

func (e *DispatcherError) Error() string {
	return fmt.Sprintf("Dispatcher failed with error: %s", e.Message)
}

// IDispatcher interface with required methods
type IDispatcher interface {
	Dispatch(ctx context.Context, alert alerts.Alert) bool
	Name() string
	String() string
}

type Dispatcher struct {
	name          string
	id            string
	url           string
	config        map[string]any
	requestHelper *helpers.RequestHelper
}

func (d *Dispatcher) Name() string {
	return d.name
}

func (d *Dispatcher) logStatus(success bool, descriptor string) {
	if success {
		logger.Printf("Successfully sent alert to %s:%s", d.url, descriptor)
	} else {
		logger.Printf("Failed to send alert to %s:%s", d.url, descriptor)
	}
}

func (d *Dispatcher) Dispatch(ctx context.Context, alert alerts.Alert) bool {
	output := d.Name()
	log.Printf("Sending dispatcher %s to %s with context: %s. Alert:\n%v", output, d.url, ctx, alert)
	descriptor := output[strings.Index(output, ":")+1:]
	var sent bool
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Exception when sending alert to %s. Alert:\n%v", output, alert)
			sent = false
		}
	}()
	d.logStatus(sent, descriptor)
	return sent
}

func NewDispatcher(name string, optFns ...DispatcherOptions) (*Dispatcher, error) {
	if name == "" {
		return nil, &DispatcherError{Message: "Invalid Dispatcher options"}
	}
	dispatcher := &Dispatcher{
		name:          name,
		requestHelper: &helpers.RequestHelper{},
	}
	for _, optFn := range optFns {
		optFn(dispatcher)
	}
	return dispatcher, nil
}
