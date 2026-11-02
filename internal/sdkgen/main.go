// Command sdkgen projects the Go protocol owners into checked-in SDK inputs.
package main

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"

	"github.com/Tangerg/frame/browser"
	"github.com/Tangerg/frame/portable"
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
	profile := struct {
		Protocol string                 `json:"protocol"`
		Limits   portable.Limits        `json:"limits"`
		Pattern  portable.PatternSyntax `json:"pattern"`
		Formats  portable.StringFormats `json:"formats"`
		Schema   portable.SchemaProfile `json:"schema"`
	}{portable.Protocol, portable.ProfileLimits(), portable.ProfilePatternSyntax(), portable.ProfileStringFormats(), portable.ProfileSchema()}
	encodedProfile, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sdk/src/protocol.generated.ts"), types.Bytes(), 0644); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "portable/profile.generated.json"), append(encodedProfile, '\n'), 0644); err != nil {
		log.Fatal(err)
	}
}
