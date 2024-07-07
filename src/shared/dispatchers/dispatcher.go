package dispatchers

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

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
	Dispatch(ctx context.Context, alert map[string]interface{}) bool
	GetName() string
}

type Dispatcher struct {
	Name          string
	URL           string
	Config        map[string]interface{}
	RequestHelper *helpers.RequestHelper
}

func (d *Dispatcher) GetName() string {
	return d.Name
}

func (d *Dispatcher) logStatus(success bool, descriptor string) {
	if success {
		logger.Printf("Successfully sent alert to %s:%s", d.URL, descriptor)
	} else {
		logger.Printf("Failed to send alert to %s:%s", d.URL, descriptor)
	}
}

func (d *Dispatcher) Dispatch(ctx context.Context, alert map[string]interface{}) bool {
	output := d.GetName()
	log.Printf("Sending dispatcher %s to %s with context: %s. Alert:\n%s", output, d.URL, ctx, alert)
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

func (d *Dispatcher) DispatchLogic(ctx context.Context, alert map[string]interface{}) bool {
	// Placeholder for actual dispatch logic
	log.Printf("Using base dispatcher %s to %s with context: %s. Alert:\n%s", d.GetName(), d.URL, ctx, alert)
	return true
}
