package value_test

import (
	"testing"

	"github.com/Tangerg/frame/value"
)

func TestNilClassificationAtInterfaceBoundaries(t *testing.T) {
	var pointer *int
	var mapping map[string]int
	var sequence []string
	var channel chan int
	var callback func()
	for _, sample := range []any{nil, pointer, mapping, sequence, channel, callback} {
		if !value.IsNil(sample) {
			t.Errorf("nil %T was treated as present", sample)
		}
	}
	for _, sample := range []any{0, false, "", struct{}{}, new(int), map[string]int{}, []string{}, make(chan int), func() {}} {
		if value.IsNil(sample) {
			t.Errorf("present %T was treated as nil", sample)
		}
	}
}
