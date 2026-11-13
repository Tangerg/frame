package value

import (
	"encoding/base64"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"strings"
	"testing"
)

func reflectorKey(pkg, name string) string {
	return "go_" + base64.RawURLEncoding.EncodeToString([]byte(pkg)) + "_" + name
}

func TestReadableDefinitionsNameTypesAndRewriteReferences(t *testing.T) {
	item, otherItem, page := reflectorKey("example.com/a/memory", "Item"), reflectorKey("example.com/b/schedule", "Item"), reflectorKey("example.com/a/memory", "Page")
	schema := `{"$ref":"#/$defs/` + page + `","$defs":{"` + page + `":{"type":"object","properties":{"rows":{"items":{"$ref":"#/$defs/` + item + `"}},"next":{"$ref":"#/$defs/` + otherItem + `"}}},"` + item + `":{"type":"string"},"` + otherItem + `":{"type":"integer"}}}`
	readable, err := ReadableDefinitions([]byte(schema))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(readable), "go_") {
		t.Fatalf("readable schema still names a reflector key: %s", readable)
	}
	var document struct {
		Ref  string                    `json:"$ref"`
		Defs map[string]jsontext.Value `json:"$defs"`
	}
	if err := json.Unmarshal(readable, &document); err != nil {
		t.Fatal(err)
	}
	if document.Ref != "#/$defs/memory.Page" {
		t.Fatalf("root ref = %q, want package and type", document.Ref)
	}
	for _, name := range []string{"memory.Page", "memory.Item", "schedule.Item"} {
		if _, found := document.Defs[name]; !found {
			t.Fatalf("definitions %v lack %s", document.Defs, name)
		}
	}
	if !strings.Contains(string(document.Defs["memory.Page"]), `"#/$defs/memory.Item"`) {
		t.Fatalf("memory.Page does not reference memory.Item: %s", document.Defs["memory.Page"])
	}
}

func TestReadableDefinitionsPreserveCollidingTypeIdentities(t *testing.T) {
	first, second := reflectorKey("example.com/a/x", "Item"), reflectorKey("example.com/b/x", "Item")
	schema := `{"$defs":{"` + first + `":{"type":"string"},"` + second + `":{"type":"integer"}}}`
	if data, err := ReadableDefinitions([]byte(schema)); err != nil || !strings.Contains(string(data), first) || !strings.Contains(string(data), second) {
		t.Fatalf("distinct declarations lost: %s, %v", data, err)
	}
}

func TestReadableDefinitionsLeavesLiteralReferencesAlone(t *testing.T) {
	key := reflectorKey("example.org/model", "Row")
	original := "#/$defs/" + key
	schema := `{"$defs":{"` + key + `":{"type":"string"}},"allOf":[{"$ref":"` + original + `"},{"const":"` + original + `"},{"enum":["` + original + `"]}]}`
	output, err := ReadableDefinitions([]byte(schema))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(output), original) != 2 || !strings.Contains(string(output), `"$ref":"#/$defs/model.Row"`) {
		t.Fatal(string(output))
	}
}

func TestReadableDefinitionsPreserveEncodedPointerFragments(t *testing.T) {
	key := reflectorKey("example.org/model", "Item")
	for _, reference := range []string{"#/%24defs/" + key, "#/$defs/%67" + key[1:]} {
		schema := []byte(`{"$defs":{"` + key + `":{"type":"object","properties":{"value":{"type":"string"}}}},"$ref":"` + reference + `/properties/value"}`)
		if _, err := AdmitSchema(schema); err != nil {
			t.Fatalf("original pointer %q: %v", reference, err)
		}
		readable, err := ReadableDefinitions(schema)
		if err != nil {
			t.Fatal(err)
		}
		projected, err := AdmitSchema(readable)
		if err != nil {
			t.Fatalf("renamed pointer %q lost its original target: %s, %v", reference, readable, err)
		}
		if projected.Validate([]byte(`"kept"`)) != nil || projected.Validate([]byte(`42`)) == nil {
			t.Fatalf("renamed pointer %q changed its target predicate", reference)
		}
	}
}
