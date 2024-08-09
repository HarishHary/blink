package helpers

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"plugin"
	"strings"
)

const DATETIME_FORMAT = "2006-01-02T15:04:05.000Z"

func JsonCompact(v any) string {
	bytes, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(bytes)
}

// EqualStringSlices checks if two string slices are equal
func EqualStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	aMap := make(map[string]struct{}, len(a))
	for _, item := range a {
		aMap[item] = struct{}{}
	}

	for _, item := range b {
		if _, found := aMap[item]; !found {
			return false
		}
	}

	return true
}

// intersect returns the intersection of two slices
func Intersect(a, b []string) []string {
	set := make(map[string]struct{})
	for _, item := range b {
		set[item] = struct{}{}
	}
	var result []string
	for _, item := range a {
		if _, found := set[item]; found {
			result = append(result, item)
		}
	}
	return result
}

// difference returns the difference of two slices (a - b)
func Difference(a, b []string) []string {
	set := make(map[string]struct{})
	for _, item := range b {
		set[item] = struct{}{}
	}
	var result []string
	for _, item := range a {
		if _, found := set[item]; !found {
			result = append(result, item)
		}
	}
	return result
}

func LoadPlugin[T any](path string) (T, error) {
	var pluginInstance T
	p, err := plugin.Open(path)
	if err != nil {
		return pluginInstance, err
	}
	sym, err := p.Lookup("Plugin")
	if err != nil {
		return pluginInstance, err
	}
	pluginInstance, ok := sym.(T)
	if !ok {
		return pluginInstance, fmt.Errorf("invalid type for plugin %s", path)
	}
	return pluginInstance, nil
}

func KeyValueListToDict(listObjects []map[string]any, key string, value string) map[string]any {
	result := make(map[string]any)
	for _, item := range listObjects {
		result[item[key].(string)] = item[value]
	}
	return result
}

func IsBase64(b64 string) string {
	if len(b64) < 12 {
		return ""
	}
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return ""
	}
	return string(decoded)
}

func DefangIOC(ioc string) string {
	return strings.ReplaceAll(ioc, ".", "[.]")
}

func IsIPInNetwork(ipAddr string, networks []string) bool {
	ip := net.ParseIP(ipAddr)
	for _, network := range networks {
		_, ipNet, err := net.ParseCIDR(network)
		if err == nil && ipNet.Contains(ip) {
			return true
		}
	}
	return false
}

func PatternMatch(stringToMatch string, pattern string) bool {
	matched, _ := filepath.Match(pattern, stringToMatch)
	return matched
}

func PatternMatchList(stringToMatch string, patterns []string) bool {
	for _, pattern := range patterns {
		if matched, _ := filepath.Match(pattern, stringToMatch); matched {
			return true
		}
	}
	return false
}

func GetValFromList(listOfDicts []map[string]any, returnFieldKey, fieldCmpKey, fieldCmpVal string) map[any]struct{} {
	valuesOfReturnField := make(map[any]struct{})
	for _, item := range listOfDicts {
		if item[fieldCmpKey] == fieldCmpVal {
			valuesOfReturnField[item[returnFieldKey]] = struct{}{}
		}
	}
	return valuesOfReturnField
}
