package frame

import "fmt"

// resolveLimit gives an unset limit its default; a negative limit is invalid.
func resolveLimit(name string, value *int, fallback int) error {
	if *value == 0 {
		*value = fallback
	}
	if *value < 1 {
		return fmt.Errorf("%w: %s limit", ErrInvalid, name)
	}
	return nil
}
