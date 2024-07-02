package dispatchers

import (
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

// OutputProperty struct equivalent to namedtuple in Python
type OutputProperty struct {
	Description       string
	Value             string
	InputRestrictions map[rune]struct{}
	MaskInput         bool
	CredRequirement   bool
}

// IDispatcher interface with required methods
type IDispatcher interface {
	DispatchLogic(alert, descriptor string) bool
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

func (d *BaseDispatcher) Dispatch(alert, output string) bool {
	log.Printf("Sending %s to %s", alert, output)
	descriptor := output[strings.Index(output, ":")+1:]
	var sent bool
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Exception when sending %s to %s. Alert:\n%v", alert, output, alert)
			sent = false
		}
	}()
	sent = d.DispatchLogic(alert, descriptor)
	d.logStatus(sent, descriptor)
	return sent
}

func (d *BaseDispatcher) DispatchLogic(alert, descriptor string) bool {
	// Placeholder for actual dispatch logic
	log.Printf("Using base dispatcher %s to %s. Alert:\n%v", alert, descriptor, alert)
	return true
}
