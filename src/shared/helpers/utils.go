package helpers

import (
	"encoding/json"
	"reflect"
)

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
