package shared

import (
	"reflect"
	"slices"

	"github.com/harishhary/blink/src/shared/helpers"
)

type Record map[string]interface{}

// getMergedKeys retrieves merge keys from a record
func (r *Record) GetMergedKeys(keys []string) map[string]interface{} {
	mergeKeys := make(map[string]interface{})
	for _, key := range keys {
		mergeKeys[key] = helpers.GetFirstKey(r, key, "N/A")
	}
	return mergeKeys
}

// cleanRecord removes ignored keys from the record
func (r *Record) CleanRecord(ignoredKeys []string) Record {
	result := make(Record)
	for key, val := range *r {
		if slices.Contains(ignoredKeys, key) {
			continue
		}
		if v, ok := val.(Record); ok {
			result[key] = v.CleanRecord(ignoredKeys)
		} else {
			result[key] = val
		}
	}
	return result
}

// computeDiff finds values in the record that are not in the common subset
func (r *Record) ComputeDiff(common map[string]interface{}) map[string]interface{} {
	diff := make(map[string]interface{})
	for key, val := range *r {
		if commonVal, ok := common[key]; !ok || !reflect.DeepEqual(val, commonVal) {
			if v, ok := val.(Record); ok && reflect.TypeOf(commonVal).Kind() == reflect.Map {
				nestedDiff := v.ComputeDiff(commonVal.(map[string]interface{}))
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
