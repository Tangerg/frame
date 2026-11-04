package browser

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	framevalue "github.com/Tangerg/frame/value"
)

// Identity, capacity and persisted evidence must use the same wire spelling.
// Escaping also preserves the maximum name expansion reserved for exit proofs.
func marshalControl(value any) ([]byte, error) {
	return json.Marshal(value, json.Deterministic(true), jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true))
}

func sameWire[T any](a, b T) bool {
	return bytes.Equal(wireBytes(a), wireBytes(b))
}

func copyWire[T any](value T) T {
	var copy T
	if err := json.Unmarshal(wireBytes(value), &copy); err != nil {
		panic(fmt.Errorf("browser: protocol value %T does not round-trip: %w", value, err))
	}
	return copy
}

func wireBytes(value any) []byte {
	encoded, err := marshalControl(value)
	if err != nil {
		panic(fmt.Errorf("browser: protocol value %T is not encodable: %w", value, err))
	}
	return encoded
}

func requestDigest(value any) (framevalue.Digest, error) {
	encoded, err := marshalControl(value)
	if err != nil {
		return framevalue.Digest{}, err
	}
	canonical, err := framevalue.ParseJSON(encoded)
	if err != nil {
		return framevalue.Digest{}, err
	}
	return canonical.Digest(), nil
}
