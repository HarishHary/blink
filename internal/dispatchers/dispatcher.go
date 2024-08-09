package dispatchers

import (
	"time"

	"github.com/google/uuid"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/pkg/alerts"
)

// Constants for retry attempts and request timeout
const (
	MaxRetryAttempts      = 5
	DefaultRequestTimeout = 3*time.Second + 50*time.Millisecond // Close to 3.05
	DefaultServiceURL     = "https://example.com/api"           // Replace with actual URL
)

// IDispatcher interface with required methods
type IDispatcher interface {
	Dispatch(alert alerts.Alert) (bool, errors.Error)

	// Getters
	Id() string
	Name() string
	Description() string

	// Methods
	String() string
}

type Dispatcher struct {
	id            string
	name          string
	description   string
	config        map[string]any
	requestHelper *helpers.RequestHelper
}

func (d *Dispatcher) Id() string {
	return d.id
}

func (d *Dispatcher) Name() string {
	return d.name
}

func (d *Dispatcher) Description() string {
	return d.description
}

func (d *Dispatcher) String() string {
	return d.name
}

func (d *Dispatcher) Dispatch(alert alerts.Alert) (bool, errors.Error) {
	// output := d.Name()
	// log.Printf("Sending dispatcher %s to %s with context: %s. Alert:\n%v", output, d.Name(), ctx, alert)
	// descriptor := output[strings.Index(output, ":")+1:]
	// var sent bool
	// defer func() {
	// 	if r := recover(); r != nil {
	// 		log.Printf("Exception when sending alert to %s. Alert:\n%v", output, alert)
	// 		sent = false
	// 	}
	// }()
	// d.logStatus(sent, descriptor)
	return true, nil
}

func NewDispatcher(name string, config map[string]any, optFns ...DispatcherOptions) (*Dispatcher, errors.Error) {
	if name == "" {
		return nil, errors.New("invalid dispatcher options")
	}
	dispatcher := &Dispatcher{
		id:            uuid.NewString(),
		name:          name,
		description:   "Unknown description",
		config:        config,
		requestHelper: &helpers.RequestHelper{},
	}
	for _, optFn := range optFns {
		optFn(dispatcher)
	}
	return dispatcher, nil
}
