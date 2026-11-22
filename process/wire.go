package process

import (
	_ "embed"
	"reflect"

	framevalue "github.com/Tangerg/frame/value"
)

//go:embed wire.json
var document []byte

// wireSpecification is the Go projection of wire.json, the one document Go and
// the Node SDK both read for protocol names and limits.
type wireSpecification struct {
	MaxFrameBytes int `json:"maxFrameBytes"`
	MaxInFlight   int `json:"maxInFlight"`
	Methods       struct {
		Hello    string `json:"hello"`
		Prepare  string `json:"prepare"`
		Activate string `json:"activate"`
		Call     string `json:"call"`
		Chunk    string `json:"chunk"`
		Cancel   string `json:"cancel"`
		Close    string `json:"close"`
	} `json:"methods"`
	Errors wireErrors `json:"errors"`
}

type wireErrors struct {
	Invalid   string `json:"invalid"`
	Capacity  string `json:"capacity"`
	Contract  string `json:"contract"`
	Unknown   string `json:"unknown"`
	Closed    string `json:"closed"`
	Cancelled string `json:"cancelled"`
}

// publishes reports whether code is one of the published error codes. The
// decoded document is the only list, so a new code needs no second enumeration.
func (e wireErrors) publishes(code string) bool {
	if code == "" {
		return false
	}
	codes := reflect.ValueOf(e)
	for index := range codes.NumField() {
		if codes.Field(index).String() == code {
			return true
		}
	}
	return false
}

var wire = func() wireSpecification {
	var spec wireSpecification
	if err := framevalue.DecodeValue(document, &spec, "wire"); err != nil {
		panic(err)
	}
	return spec
}()
