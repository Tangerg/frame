package frame

import (
	"fmt"
)

// ResolveLimit applies a configuration's zero-means-default convention and
// refuses a negative bound before any owner records against it.
func ResolveLimit(name string, value, fallback int) (int, error) {
	if value == 0 {
		return fallback, nil
	}
	if value < 1 {
		return 0, fmt.Errorf("%w: %s limit", ErrInvalid, name)
	}
	return value, nil
}
