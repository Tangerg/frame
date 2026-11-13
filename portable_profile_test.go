package frame_test

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/Tangerg/frame"
)

func TestGeneratedRuntimeProfileMatchesOwner(t *testing.T) {
	data, err := os.ReadFile("contract-profile.generated.json")
	if err != nil {
		t.Fatal(err)
	}
	var generated frame.PortableProfile
	if err := json.Unmarshal(data, &generated); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(generated, frame.CurrentPortableProfile()) {
		t.Fatal("runtime profile projection is stale; run go generate ./scripts")
	}
}
