package elastic

type Configuration struct {
	CloudID     string `env:"KS_BDR_CLOUD_ID"`
	APIKey      string `env:"KS_BDR_API_TOKEN"`
	Environment string `env:"KS_BDR_API_TOKEN"`
	Tenant      string
}
