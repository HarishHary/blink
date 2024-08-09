package azure_storage

import "fmt"

type Configuration struct {
	Username string `env:"KS_SVC_AZURESTORAGE_USERNAME"`
	Password string `env:"KS_SVC_AZURESTORAGE_PASSWORD"`
}

func (config *Configuration) connectionString() string {
	return fmt.Sprintf(
		"DefaultEndpointsProtocol=https;AccountName=%s;AccountKey=%s;EndpointSuffix=core.windows.net",
		config.Username, config.Password,
	)
}
