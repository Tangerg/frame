package distribution

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func TestReadBoundedAdmitsTheFullPositiveLimitRange(t *testing.T) {
	for _, limit := range []int64{3, 4, math.MaxInt64 - 1, math.MaxInt64} {
		got, err := readBounded(t.Context(), strings.NewReader("abc"), limit)
		if err != nil || string(got) != "abc" {
			t.Fatalf("limit %d changed admitted bytes to %q: %v", limit, got, err)
		}
	}
	for _, limit := range []int64{-1, 0, 2} {
		if _, err := readBounded(t.Context(), strings.NewReader("abc"), limit); !errors.Is(err, ErrCapacity) {
			t.Fatalf("limit %d admitted excessive bytes: %v", limit, err)
		}
	}
	if got, err := readBounded(t.Context(), strings.NewReader(""), 0); err != nil || len(got) != 0 {
		t.Fatalf("zero-length input exceeded its exact limit: %q, %v", got, err)
	}
}
