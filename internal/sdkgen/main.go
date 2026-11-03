// Command sdkgen projects the Go protocol owners into checked-in SDK inputs.
package main

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"

	"github.com/Tangerg/frame/browser"
	"github.com/Tangerg/frame/internal/localfile"
	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/process"
)

//go:generate go run . ../..

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: sdkgen repository-root")
	}
	root := os.Args[1]
	var types bytes.Buffer
	if err := browser.WriteTypeScript(&types); err != nil {
		log.Fatal(err)
	}
	profile := portable.CurrentRuntimeProfile()
	encodedProfile, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sdk/src/protocol.generated.ts"), types.Bytes(), localfile.PublicFileMode); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "portable/profile.generated.json"), append(encodedProfile, '\n'), localfile.PublicFileMode); err != nil {
		log.Fatal(err)
	}
	encodedShapes, err := json.MarshalIndent(process.Shapes(), "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "process/shapes.generated.json"), append(encodedShapes, '\n'), localfile.PublicFileMode); err != nil {
		log.Fatal(err)
	}
}
