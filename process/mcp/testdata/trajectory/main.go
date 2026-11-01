// Build this fixture in the upstream workspace so the callback contract and
// sample session use the original owner's schema and encoding.
package main

import (
	"encoding/json"
	"log"
	"os"

	"github.com/Tangerg/flame/packages/go-sdk/plugin"
	"github.com/Tangerg/flame/packages/go-sdk/protocol"
	"github.com/Tangerg/qimu/portable"
)

func main() {
	read, err := plugin.ReadDocumentRangeMethod()
	if err != nil {
		log.Fatal(err)
	}
	err = json.NewEncoder(os.Stdout).Encode(struct {
		Read     portable.MethodDescriptor `json:"read"`
		Document protocol.SessionArtifact  `json:"document"`
	}{read.Descriptor(), protocol.SessionArtifact{Version: protocol.SessionArtifactVersion, Session: protocol.ArtifactSession{Title: "Frame integration"}}})
	if err != nil {
		log.Fatal(err)
	}
}
