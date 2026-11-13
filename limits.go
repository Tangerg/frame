package frame

import (
	"fmt"
)

func ResolveLimit(name string, value *int, fallback int) error {
	if value == nil {
		return ErrInvalid
	}
	if *value == 0 {
		*value = fallback
	}
	if *value < 1 {
		return fmt.Errorf("%w: %s limit", ErrInvalid, name)
	}
	return nil
}
