package helpers

import (
	"encoding/json"
	"fmt"
	"plugin"
	"reflect"
)

const DATETIME_FORMAT = "2006-01-02T15:04:05.000Z"

func GetFirstKey(data interface{}, searchKey string, defaultValue interface{}) interface{} {
	keys := GetKeys(data, searchKey, 1)
	if len(keys) > 0 {
		return keys[0]
	}
	return defaultValue
}

// GetKeys searches for a key anywhere in the nested data structure, returning all associated values.
func GetKeys(data interface{}, searchKey string, maxMatches int) []interface{} {
	// Recursion is generally inefficient due to stack shuffling for each function call/return.
	// Instead, we use a slice as a stack to avoid recursion.
	type container struct {
		data interface{}
	}

	containers := []container{{data: data}}
	results := []interface{}{}

	for len(containers) > 0 {
		// Pop the last element
		lastIndex := len(containers) - 1
		current := containers[lastIndex]
		containers = containers[:lastIndex]

		switch obj := current.data.(type) {
		case map[string]interface{}:
			if value, found := obj[searchKey]; found {
				results = append(results, value)
				if maxMatches > 0 && len(results) == maxMatches {
					// We found n matches - return early
					return results
				}
			}
			// Enqueue all nested dicts and lists for further searching
			for _, val := range obj {
				if val != nil && isContainerType(val) {
					containers = append(containers, container{data: val})
				}
			}
		case []interface{}:
			// Obj is a list - enqueue all nested dicts and lists for further searching
			for _, val := range obj {
				if val != nil && isContainerType(val) {
					containers = append(containers, container{data: val})
				}
			}
		}
	}
	return results
}

// isContainerType checks if the value is a container type (map or slice)
func isContainerType(val interface{}) bool {
	return reflect.TypeOf(val).Kind() == reflect.Map || reflect.TypeOf(val).Kind() == reflect.Slice
}

func JsonCompact(v interface{}) string {
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

func LoadPlugins[T any](paths []string) ([]T, error) {
	var plugins []T
	for _, path := range paths {
		p, err := plugin.Open(path)
		if err != nil {
			return nil, err
		}
		sym, err := p.Lookup("Plugin")
		if err != nil {
			// return nil, err
			continue
		}
		pluginInstance, ok := sym.(T)
		if !ok {
			return nil, fmt.Errorf("invalid type for plugin %s", path)
		}
		plugins = append(plugins, pluginInstance)
	}
	return plugins, nil
}
