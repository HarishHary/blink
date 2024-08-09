package configuration

import "fmt"

type ServiceConfiguration struct {
	Name        string `env:"APP_NAME"`
	Environment string `env:"APP_STACK_ENVIRONMENT"`

	Tenant string `env:"TENANT"`
	Domain string `env:"DOMAIN"`

	Token       string `file:"/var/run/secrets/kubernetes.io/serviceaccount/token"`
	Certificate string `file:"/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"`

	Kubernetes Kubernetes
	JWT        JWT
}

// ServiceRole returns the role used by the service to perform operations
func (configuration *ServiceConfiguration) ServiceRole() string {
	return fmt.Sprintf("%s-%s-admin", configuration.Name, configuration.Tenant)
}

type JWT struct {
	Product  string `env:"PRODUCT"`
	Audience string `env:"AUTH0_AUDIENCE"`
	Issuer   string `env:"AUTH0_ISSUER"`
}

type Kubernetes struct {
	Target string `env:"KUBERNETES_SERVICE_HOST"`
	Port   uint   `env:"KUBERNETES_SERVICE_PORT"`
}
