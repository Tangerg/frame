package process

import (
	_ "embed"
	json "encoding/json/v2"
)

//go:embed wire.json
var document []byte

// wireSpecification is the Go projection of wire.json, the one document Go and
// the Node SDK both read for protocol names and limits.
type wireSpecification struct {
	Version       int `json:"version"`
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
	Errors struct {
		Invalid   string `json:"invalid"`
		Capacity  string `json:"capacity"`
		Contract  string `json:"contract"`
		Unknown   string `json:"unknown"`
		Closed    string `json:"closed"`
		Cancelled string `json:"cancelled"`
	} `json:"errors"`
}

var wire = func() wireSpecification {
	var spec wireSpecification
	if err := json.Unmarshal(document, &spec, json.RejectUnknownMembers(true)); err != nil {
		panic(err)
	}
	return spec
}()
