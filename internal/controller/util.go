package controller

import (
	"encoding/json"
	"reflect"
)

// equalJSON compares two values by their JSON form, which normalises the
// int/int64/float64 differences between typed and unstructured content.
func equalJSON(a, b interface{}) bool {
	ab, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	var av, bv interface{}
	if json.Unmarshal(ab, &av) != nil || json.Unmarshal(bb, &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}
