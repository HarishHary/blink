package dispatchers

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/harishhary/blink/src/helpers"
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
	Dispatch(ctx context.Context, alert map[string]interface{}) bool
}

type BaseDispatcher struct {
	ServiceName   string
	ServiceURL    string
	Config        map[string]interface{}
	RequestHelper *helpers.RequestHelper
}

func (d *BaseDispatcher) logStatus(success bool, descriptor string) {
	if success {
		logger.Printf("Successfully sent alert to %s:%s", d.ServiceURL, descriptor)
	} else {
		logger.Printf("Failed to send alert to %s:%s", d.ServiceURL, descriptor)
	}
}

func (d *BaseDispatcher) Dispatch(ctx context.Context, alert map[string]interface{}) bool {
	output := d.ServiceName
	log.Printf("Sending dispatcher %s to %s with context: %s. Alert:\n%s", d.ServiceName, d.ServiceURL, ctx, alert)
	descriptor := output[strings.Index(output, ":")+1:]
	var sent bool
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Exception when sending %s to %s. Alert:\n%v", alert, output, alert)
			sent = false
		}
	}()
	sent = d.DispatchLogic(ctx, alert)
	d.logStatus(sent, descriptor)
	return sent
}

func (d *BaseDispatcher) DispatchLogic(ctx context.Context, alert map[string]interface{}) bool {
	// Placeholder for actual dispatch logic
	log.Printf("Using base dispatcher %s to %s with context: %s. Alert:\n%s", d.ServiceName, d.ServiceURL, ctx, alert)
	return true
}
