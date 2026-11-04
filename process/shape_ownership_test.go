package process

import (
	"reflect"
	"testing"
)

func TestShapeProjectionCannotChangeParserAdmission(t *testing.T) {
	raw := []byte(`{"id":"1","method":"hello","params":{}}`)
	if _, err := parseEnvelope(raw); err != nil {
		t.Fatal(err)
	}
	expected := Shapes()
	projection := Shapes()
	projection.Request.Required[0] = "changed"
	if _, err := parseEnvelope(raw); err != nil {
		t.Fatalf("projection changed live admission: %v", err)
	}
	if !reflect.DeepEqual(Shapes(), expected) {
		t.Fatal("projection changed subsequent shape reads")
	}
}
