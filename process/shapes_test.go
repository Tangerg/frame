package process_test

import (
	json "encoding/json/v2"
	"os"
	"reflect"
	"testing"

	"github.com/Tangerg/frame/process"
)

func TestGeneratedWireProfileMatchesOwners(t *testing.T) {
	data, err := os.ReadFile("wire-profile.generated.json")
	if err != nil {
		t.Fatal(err)
	}
	var generated process.WireProfile
	if err := json.Unmarshal(data, &generated, json.RejectUnknownMembers(true)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(generated, process.CurrentWireProfile()) {
		t.Fatal("wire profile projection is stale; run go generate ./scripts")
	}
}
