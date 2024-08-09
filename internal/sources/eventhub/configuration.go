package eventhub

import (
	"fmt"
)

type Configuration struct {
	Hostname      string `env:"SVC_EVENTHUB_HOST"`
	Username      string `env:"SVC_EVENTHUB_USERNAME"`
	Password      string `env:"SVC_EVENTHUB_PASSWORD"`
	Partition     string
	ConsumerGroup string
	EventHubName  string
}

func (config *Configuration) connectionString() string {
	return fmt.Sprintf(
		"Endpoint=sb://%s/;SharedAccessKeyName=%s;SharedAccessKey=%s;EntityPath=%s",
		config.Hostname, config.Username, config.Password, config.EventHubName,
	)
}
