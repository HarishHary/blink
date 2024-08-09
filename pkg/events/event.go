package events

import (
	"reflect"
	"slices"
)

type Event map[string]any

// getMergedKeys retrieves merge keys from a Event
func (e Event) GetMergedKeys(keys []string) map[string]any {
	mergeKeys := make(map[string]any)
	for _, key := range keys {
		mergeKeys[key] = e.GetFirstKey(key, "N/A")
	}
	return mergeKeys
}

// cleanEvent removes ignored keys from the Event
func (e Event) CleanEvent(ignoredKeys []string) Event {
	result := make(Event)
	for key, val := range e {
		if slices.Contains(ignoredKeys, key) {
			continue
		}
		if v, ok := val.(Event); ok {
			result[key] = v.CleanEvent(ignoredKeys)
		} else {
			result[key] = val
		}
	}
	return result
}

// computeDiff finds values in the Event that are not in the common subset
func (e Event) ComputeDiff(common map[string]any) map[string]any {
	diff := make(map[string]any)
	for key, val := range e {
		if commonVal, ok := common[key]; !ok || !reflect.DeepEqual(val, commonVal) {
			if v, ok := val.(Event); ok && reflect.TypeOf(commonVal).Kind() == reflect.Map {
				nestedDiff := v.ComputeDiff(commonVal.(map[string]any))
				if len(nestedDiff) > 0 {
					diff[key] = nestedDiff
				}
			} else {
				diff[key] = val
			}
		}
	}
	return diff
}

func (e Event) DeepGet(keys []string, defaultValue any) any {
	var current any = e
	for _, key := range keys {
		if dict, ok := current.(map[string]any); ok {
			if value, found := dict[key]; found {
				current = value
			} else {
				return defaultValue
			}
		} else {
			return defaultValue
		}
	}
	if current == nil {
		return defaultValue
	}
	return current
}

func (e Event) DeepWalk(keys []string, defaultValue any, returnVal string) any {
	found := map[any]struct{}{}
	var walk func(obj any, keys []string) any

	walk = func(obj any, keys []string) any {
		if len(keys) == 0 {
			if reflect.ValueOf(obj).IsZero() {
				return defaultValue
			}
			return obj
		}

		currentKey := keys[0]

		if dict, ok := obj.(map[string]any); ok {
			if nextObj, found := dict[currentKey]; found {
				return walk(nextObj, keys[1:])
			}
			return defaultValue
		}

		if arr, ok := obj.([]any); ok {
			for _, item := range arr {
				if val := walk(item, keys); val != defaultValue {
					if arrVal, ok := val.([]any); ok {
						for _, subItem := range arrVal {
							found[subItem] = struct{}{}
						}
					} else {
						found[val] = struct{}{}
					}
				}
			}
		}
		return defaultValue
	}

	walk(e, keys)
	foundList := []any{}
	for key := range found {
		foundList = append(foundList, key)
	}

	switch returnVal {
	case "first":
		if len(foundList) > 0 {
			return foundList[0]
		}
	case "last":
		if len(foundList) > 0 {
			return foundList[len(foundList)-1]
		}
	case "all":
		if len(foundList) == 1 {
			return foundList[0]
		}
		return foundList
	}

	return defaultValue
}

func (e Event) Get(key string, defaultValue any) any {
	if value, exists := e[key]; exists {
		return value
	}
	return defaultValue
}

func (e Event) GetFirstKey(key string, defaultValue any) any {
	keys := e.GetKeys(key, 1)
	if len(keys) > 0 {
		return keys[0]
	}
	return defaultValue
}

// isContainerType checks if the value is a container type (map or slice)
func isContainerType(val any) bool {
	return reflect.TypeOf(val).Kind() == reflect.Map || reflect.TypeOf(val).Kind() == reflect.Slice
}

// GetKeys searches for a key anywhere in the nested data structure, returning all associated values.
func (e Event) GetKeys(key string, maxMatches int) []any {
	// Recursion is generally inefficient due to stack shuffling for each function call/return.
	// Instead, we use a slice as a stack to avoid recursion.
	type container struct {
		data any
	}

	containers := []container{{data: e}}
	results := []any{}

	for len(containers) > 0 {
		// Pop the last element
		lastIndex := len(containers) - 1
		current := containers[lastIndex]
		containers = containers[:lastIndex]

		switch obj := current.data.(type) {
		case map[string]any:
			if value, found := obj[key]; found {
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
		case []any:
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
