// Command generate projects the Go protocol owners into checked-in SDK inputs.
package main

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
	"github.com/Tangerg/frame/internal/localfile"
	"github.com/Tangerg/frame/process"
)

//go:generate go run . ..

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: generate repository-root")
	}
	if err := generate(os.Args[1]); err != nil {
		log.Fatal(err)
	}
}

func generate(root string) error {
	var types bytes.Buffer
	if err := browser.WriteTypeScript(&types); err != nil {
		return err
	}
	profile := frame.CurrentPortableProfile()
	encodedProfile, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return err
	}
	encodedShapes, err := json.MarshalIndent(process.Shapes(), "", "  ")
	if err != nil {
		return err
	}
	projections := []struct {
		path string
		body []byte
	}{
		{"sdk/src/protocol.generated.ts", types.Bytes()},
		{"contract-profile.generated.json", append(encodedProfile, '\n')},
		{"process/shapes.generated.json", append(encodedShapes, '\n')},
	}
	for _, projection := range projections {
		if err := localfile.WriteGenerated(filepath.Join(root, projection.path), projection.body); err != nil {
			return err
		}
	}
	return nil
}
