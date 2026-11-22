package value

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"testing"
)

func TestObjectRepresentationCannotChangeRequestIdentity(t *testing.T) {
	typed := struct {
		Z string         `json:"z"`
		A map[string]int `json:"a"`
	}{"same", map[string]int{"b": 2, "a": 1}}
	a, err := EncodeJSON(typed)
	if err != nil {
		t.Fatal(err)
	}
	b, err := EncodeJSON(map[string]any{"a": map[string]int{"a": 1, "b": 2}, "z": "same"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseJSON([]byte(` { "z":"\u0073ame", "a": {"b":2,"a":1} } `))
	if err != nil {
		t.Fatal(err)
	}
	if a != b || a != c || a.Digest() != c.Digest() {
		t.Fatalf("equivalent inputs changed identity: %s, %s, %s", a.String(), b.String(), c.String())
	}
	raw := a.Bytes()
	raw[0] = 'x'
	if a.String() != `{"a":{"a":1,"b":2},"z":"same"}` {
		t.Fatal("mutable projection changed admitted input")
	}
}

func TestNumericLexemesAndArrayOrderRemainExact(t *testing.T) {
	values := []string{`{"n":9007199254740992}`, `{"n":9007199254740993}`, `{"n":1}`, `{"n":1.0}`, `{"n":1e0}`, `{"n":-0}`, `{"n":0}`, `[1,2]`, `[2,1]`}
	seen := map[Digest]bool{}
	for _, value := range values {
		input, err := ParseJSON([]byte(value))
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(input)
		if err != nil || string(encoded) != value || seen[input.Digest()] {
			t.Fatalf("exact input collapsed or changed: %s, %s, %v", value, encoded, err)
		}
		seen[input.Digest()] = true
		var restored JSON
		if err := json.Unmarshal(encoded, &restored); err != nil || restored != input {
			t.Fatalf("round trip = %s, %v", restored.String(), err)
		}
	}
}

func TestInvalidOrMissingInputCannotAcquireIdentity(t *testing.T) {
	for _, value := range []string{"", `{`, `{"a":1,"a":2}`, string([]byte{'"', 0xff, '"'}), `1 2`} {
		if _, err := ParseJSON([]byte(value)); err == nil {
			t.Fatalf("invalid JSON admitted: %q", value)
		}
	}
	var input JSON
	if input.Validate() == nil || !input.Digest().IsZero() {
		t.Fatal("missing input validated")
	}
	if _, err := json.Marshal(input); err == nil {
		t.Fatal("missing input encoded")
	}
	if _, err := EncodeJSON(jsontext.Value(`{"a":1,"a":2}`)); err == nil {
		t.Fatal("duplicate raw members admitted")
	}
}

func TestDecodeAppliesTheCodecPolicyOfAdmittedValues(t *testing.T) {
	type input struct {
		ID    uint64 `json:"id"`
		Label string `json:"label,omitzero"`
	}
	admitted, err := ParseJSON([]byte(`{"id":18446744073709551615}`))
	if err != nil {
		t.Fatal(err)
	}
	var decoded input
	if err := admitted.Decode(&decoded); err != nil || decoded.ID != 18446744073709551615 {
		t.Fatalf("exact machine integer: %+v %v", decoded, err)
	}
	for _, document := range []string{`{"id":1,"extra":true}`, `{"label":"missing id"}`, `{"id":1.5}`} {
		value, err := ParseJSON([]byte(document))
		if err != nil {
			t.Fatal(err)
		}
		previous := decoded
		if err := value.Decode(&decoded); err == nil || decoded != previous {
			t.Errorf("%s decoded or changed its destination: %+v %v", document, decoded, err)
		}
	}
	if err := (JSON{}).Decode(&decoded); err == nil {
		t.Error("absent JSON decoded")
	}
}
