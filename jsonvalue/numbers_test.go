package jsonvalue_test

import (
	jsonv1 "encoding/json"
	json "encoding/json/v2"
	"testing"

	"github.com/Tangerg/frame/jsonvalue"
)

// TestNumbersKeepEveryLiteralItDecoded pins the reason this option exists: the
// values Runtime routes through an any are tool-call identifiers and schema
// bounds, and float64 either rounds them or refuses them.
func TestNumbersKeepEveryLiteralItDecoded(t *testing.T) {
	t.Parallel()
	const document = `{"id":9007199254740993,"bound":1e400,"nested":{"list":[1,2.50,-0]},` +
		`"text":"x","flag":true,"absent":null}`

	var decoded any
	if err := json.Unmarshal([]byte(document), &decoded, jsonvalue.Numbers()); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	object, isObject := decoded.(map[string]any)
	if !isObject {
		t.Fatalf("decoded %T, want map[string]any", decoded)
	}
	for member, want := range map[string]string{"id": "9007199254740993", "bound": "1e400"} {
		number, isNumber := object[member].(jsonv1.Number)
		if !isNumber {
			t.Fatalf("member %q decoded %T, want json.Number", member, object[member])
		}
		if number.String() != want {
			t.Errorf("member %q = %s, want %s", member, number, want)
		}
	}
	// Depth is the case a decoder-wide switch covers for free and a per-value
	// unmarshaler has to be checked for.
	list, isList := object["nested"].(map[string]any)["list"].([]any)
	if !isList || len(list) != 3 {
		t.Fatalf("nested list = %#v", object["nested"])
	}
	if number, isNumber := list[2].(jsonv1.Number); !isNumber || number.String() != "-0" {
		t.Errorf("nested element = %#v, want json.Number(-0)", list[2])
	}

	// Other kinds must reach their ordinary Go representations.
	if object["text"] != "x" || object["flag"] != true || object["absent"] != nil {
		t.Errorf("non-numeric members changed: %#v", object)
	}

	// The literal survives the round trip, which is what makes a decoded
	// document safe to hash, compare, and persist.
	encoded, err := json.Marshal(decoded, json.Deterministic(true))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"absent":null,"bound":1e400,"flag":true,"id":9007199254740993,` +
		`"nested":{"list":[1,2.50,-0]},"text":"x"}`
	if string(encoded) != want {
		t.Errorf("re-encoded\n got %s\nwant %s", encoded, want)
	}
}

// TestNumbersLeaveTypedDestinationsAlone keeps the option from becoming a second
// decoding mode: a caller that named a concrete type still gets that type.
func TestNumbersLeaveTypedDestinationsAlone(t *testing.T) {
	t.Parallel()
	var typed struct {
		Count int     `json:"count"`
		Ratio float64 `json:"ratio"`
	}
	if err := json.Unmarshal([]byte(`{"count":7,"ratio":0.5}`), &typed, jsonvalue.Numbers()); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if typed.Count != 7 || typed.Ratio != 0.5 {
		t.Errorf("typed destination = %+v", typed)
	}
}

// TestDefaultDecodingStillLosesTheseNumbers records why the option is not
// optional. If this ever stops holding, exactjson has no reason to exist.
func TestDefaultDecodingStillLosesTheseNumbers(t *testing.T) {
	t.Parallel()
	var rounded any
	if err := json.Unmarshal([]byte(`9007199254740993`), &rounded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rounded == any(float64(9007199254740993)) && rounded != any(9007199254740993) {
		return
	}
	t.Errorf("encoding/json/v2 decoded an exact integer into %#v; exactjson may be obsolete", rounded)
}
