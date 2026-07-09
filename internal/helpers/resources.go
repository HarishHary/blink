package helpers

import (
	"fmt"
	"maps"
	"strings"
)

var REQUIRED_OUTPUTS = map[string]map[string]string{
	"aws-firehose": {
		"alerts": "{prefix}_streamalert_alert_delivery",
	},
}

// GetRequiredOutputs iterates through the required outputs and collapses to the right format
func GetRequiredOutputs() []string {
	var requiredOutputs []string
	for service, value := range REQUIRED_OUTPUTS {
		for output := range value {
			requiredOutputs = append(requiredOutputs, fmt.Sprintf("%s:%s", service, output))
		}
	}
	return requiredOutputs
}

// MergeRequiredOutputs iterates through the required outputs and merges them with the user outputs
func MergeRequiredOutputs(userConfig map[string]map[string]string, prefix string) map[string]map[string]string {
	config := make(map[string]map[string]string)
	maps.Copy(config, userConfig)

	for service, value := range REQUIRED_OUTPUTS {
		// Format the resource with the prefix value
		formattedValue := make(map[string]string)
		for output, resource := range value {
			formattedValue[output] = strings.Replace(resource, "{prefix}", prefix, -1)
		}

		// Add the outputs for this service if none are defined
		if _, exists := config[service]; !exists {
			config[service] = formattedValue
			continue
		}

		// Merge the outputs with existing ones for this service
		maps.Copy(config[service], formattedValue)
	}

	return config
}
