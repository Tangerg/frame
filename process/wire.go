package process

import (
	_ "embed"
	"encoding/json"
)

//go:embed wire.json
var document []byte

type wireSpecification struct {
	Version       int
	MaxFrameBytes int
	MaxInFlight   int
	Methods       struct{ Hello, Prepare, Activate, Call, Chunk, Cancel, Close string }
	Errors        struct{ Invalid, Capacity, Contract, Unknown, Closed, Cancelled string }
}

var wire = func() wireSpecification {
	var spec wireSpecification
	if err := json.Unmarshal(document, &spec); err != nil {
		panic(err)
	}
	return spec
}()
