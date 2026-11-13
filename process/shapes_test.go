package process_test

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/Tangerg/frame/process"
)

func TestGeneratedWireShapesMatchOwners(t *testing.T) {
	data, err := os.ReadFile("shapes.generated.json")
	if err != nil {
		t.Fatal(err)
	}
	var generated process.WireShapes
	if err := json.Unmarshal(data, &generated); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(generated, process.Shapes()) {
		t.Fatal("wire shapes projection is stale; run go generate ./scripts")
	}
}
