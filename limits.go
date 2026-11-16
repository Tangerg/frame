package frame

import (
	"fmt"
)

// Bound is every integer kind a configured limit takes, including durations.
type Bound interface {
	~int | ~int64 | ~uint64
}

// ResolveLimit applies a configuration's zero-means-default convention and
// refuses a negative bound before any owner records against it.
func ResolveLimit[N Bound](name string, value, fallback N) (N, error) {
	if value == 0 {
		return fallback, nil
	}
	if value < 1 {
		return 0, fmt.Errorf("%w: %s limit", ErrInvalid, name)
	}
	return value, nil
}
