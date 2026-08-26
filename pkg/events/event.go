package events

import (
	"reflect"
	"slices"

	"github.com/harishhary/blink/internal/runtime"
)

// Event is a dynamic detection record: a nested map of arbitrary fields with lookup/merge helpers.
type Event map[string]any

// RolloutKey is the side this event routes to, its tenant normalized so an event without one still routes somewhere.
func (e Event) RolloutKey() string { return runtime.NormalizeRolloutKey(e["tenant_id"]) }

// Clone returns a deep copy of the event's JSON-like maps and slices.
func (e Event) Clone() Event {
	if e == nil {
		return nil
	}
	clone := make(Event, len(e))
	for key, value := range e {
		clone[key] = cloneValue(value)
	}
	return clone
}

// CloneEvents returns event copies in the same order, each independently owned.
func CloneEvents(in []Event) []Event {
	out := make([]Event, len(in))
	for i, event := range in {
		out[i] = event.Clone()
	}
	return out
}

// cloneValue deep-copies the maps and slices inside a value, leaving scalars as they are.
func cloneValue(value any) any {
	switch value := value.(type) {
	case Event:
		return value.Clone()
	case map[string]any:
		clone := make(map[string]any, len(value))
		for key, nested := range value {
			clone[key] = cloneValue(nested)
		}
		return clone
	case []any:
		clone := make([]any, len(value))
		for i, nested := range value {
			clone[i] = cloneValue(nested)
		}
		return clone
	default:
		return value
	}
}

// GetMergedKeys is the values behind the merge keys, for building a merge key.
func (e Event) GetMergedKeys(keys []string) map[string]any {
	mergeKeys := make(map[string]any)
	for _, key := range keys {
		mergeKeys[key] = e.GetFirstKey(key, "N/A")
	}
	return mergeKeys
}

// CleanEvent returns the event without the ignored keys.
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

// ComputeDiff is the event's values that are not in the common subset.
func (e Event) ComputeDiff(common map[string]any) map[string]any {
	diff := make(map[string]any)
	for key, val := range e {
		if commonVal, ok := common[key]; !ok || !reflect.DeepEqual(val, commonVal) {
			if v, ok := val.(Event); ok {
				commonMap, commonIsMap := commonVal.(map[string]any)
				if !commonIsMap {
					diff[key] = val
					continue
				}
				nestedDiff := v.ComputeDiff(commonMap)
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

// DeepGet returns the value at the nested key path (each key a level down), or defaultValue if any level is missing.
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

// DeepWalk searches the nested structure along keys, lists included, for the first/last/all matches per returnVal.
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

// Get returns the top-level value for key, or defaultValue if absent.
func (e Event) Get(key string, defaultValue any) any {
	if value, exists := e[key]; exists {
		return value
	}
	return defaultValue
}

// GetFirstKey returns the first value found anywhere in the nested structure for key, or defaultValue.
func (e Event) GetFirstKey(key string, defaultValue any) any {
	keys := e.GetKeys(key, 1)
	if len(keys) > 0 {
		return keys[0]
	}
	return defaultValue
}

// isContainerType reports whether the value is a map or a slice.
func isContainerType(val any) bool {
	valueType := reflect.TypeOf(val)
	return valueType != nil && (valueType.Kind() == reflect.Map || valueType.Kind() == reflect.Slice)
}

// GetKeys searches for a key anywhere in the nested data structure, returning all associated values.
func (e Event) GetKeys(key string, maxMatches int) []any {
	// A slice as an explicit stack, since recursion pays a call and return per level.
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

		// A type switch matches the exact dynamic type, so a nested Event has to be normalized here or its keys go unsearched.
		data := current.data
		if ev, ok := data.(Event); ok {
			data = map[string]any(ev)
		}

		switch obj := data.(type) {
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
