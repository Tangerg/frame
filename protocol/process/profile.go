// Package wire owns the language-neutral local process profile.
package wire

import (
	_ "embed"
	"encoding/json"
)

//go:embed wire.json
var document []byte

type Specification struct {
	Version       int
	MaxFrameBytes int
	MaxInFlight   int
	Methods       struct{ Hello, Prepare, Activate, Call, Chunk, Cancel, Close string }
	Errors        struct{ Invalid, Capacity, Contract, Unknown, Closed, Cancelled string }
}

var current = func() Specification {
	var spec Specification
	if err := json.Unmarshal(document, &spec); err != nil {
		panic(err)
	}
	return spec
}()

func Current() Specification { return current }
