package value_test

import (
	"testing"

	"github.com/Tangerg/frame/value"
)

func TestShapeAdmitsEachMemberOnce(t *testing.T) {
	shape := value.Shape{Required: []string{"id", "method"}, Optional: []string{"params"}}
	for _, test := range []struct {
		name    string
		present []string
		admit   bool
	}{
		{"required", []string{"id", "method"}, true},
		{"reordered", []string{"params", "method", "id"}, true},
		{"missing", []string{"id"}, false},
		{"duplicate replaces required", []string{"id", "id"}, false},
		{"duplicate optional", []string{"id", "method", "params", "params"}, false},
		{"unknown", []string{"id", "method", "extra"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := shape.Admits(test.present); got != test.admit {
				t.Fatalf("Admits(%q) = %v, want %v", test.present, got, test.admit)
			}
		})
	}
}
