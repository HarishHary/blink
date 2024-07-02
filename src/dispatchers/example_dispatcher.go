package dispatchers

import (
	"crypto/tls"
	"net/http"

	"github.com/harishhary/blink/src/helpers"
)

type ExampleDispatcher struct {
	BaseDispatcher
}

// NewExampleDispatcher creates a new instance of ExampleDispatcher
func NewExampleDispatcher(config map[string]interface{}) *ExampleDispatcher {
	return &ExampleDispatcher{
		BaseDispatcher: BaseDispatcher{
			ServiceURL:    "https://example.com/api", // Replace with the actual URL
			Config:        config,
			RequestHelper: &helpers.RequestHelper{},
		},
	}
}

func (d *ExampleDispatcher) DispatchLogic(alert, descriptor string) bool {
	logger.Printf("Sending alert to %s:%s", d.ServiceURL, descriptor)
	headers := map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer example_token", // Replace with actual token
	}

	data := map[string]string{
		"alert":      alert,
		"descriptor": descriptor,
	}

	resp, err := d.RequestHelper.PostRequestRetry(d.ServiceURL, headers, data, d.RequestHelper.CatchExceptions())
	if err != nil {
		logger.Printf("Failed to send alert: %v", err)
		return false
	}

	success := d.RequestHelper.CheckHTTPResponse(resp)
	return success
}

// ExampleUsage demonstrates how to use the ExampleDispatcher
func ExampleUsage() {
	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	// Configuration for the dispatcher
	config := map[string]interface{}{
		"example_key": "example_value",
	}

	// Create a new instance of ExampleDispatcher
	dispatcher := NewExampleDispatcher(config)

	// Example alert and descriptor
	alert := "This is a test alert"
	descriptor := "example_descriptor"

	// Dispatch the alert using the ExampleDispatcher
	success := dispatcher.Dispatch(alert, descriptor)
	if success {
		logger.Println("Alert dispatched successfully")
	} else {
		logger.Println("Failed to dispatch alert")
	}
}
