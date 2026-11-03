// Package nilness detects a nil hidden behind an interface value: a typed nil
// pointer, map, slice, channel or function compares unequal to nil, yet any
// call through it fails.
package nilness

import "reflect"

func Is(value any) bool {
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() {
		return true
	}
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
