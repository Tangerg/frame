package value_test

import (
	jsonv1 "encoding/json"
	json "encoding/json/v2"
	"testing"

	"github.com/Tangerg/frame/value"
)

func TestNumberCodecPreservesExactLexemesAcrossBothJSONAPIs(t *testing.T) {
	for _, literal := range []string{"0", "-0", "2.50", "1e0", "9007199254740993", "18446744073709551616", "1e400"} {
		t.Run(literal, func(t *testing.T) {
			number, err := value.ParseNumber([]byte(literal))
			if err != nil {
				t.Fatal(err)
			}
			if number.String() != literal {
				t.Fatalf("number changed %s", number.String())
			}
			for _, codec := range []struct {
				marshal   func(any) ([]byte, error)
				unmarshal func([]byte, any) error
			}{
				{jsonv1.Marshal, jsonv1.Unmarshal},
				{func(v any) ([]byte, error) { return json.Marshal(v) }, func(b []byte, v any) error { return json.Unmarshal(b, v) }},
			} {
				var decoded struct {
					Value value.Number `json:"value"`
				}
				wire := []byte(`{"value":` + literal + `}`)
				if err := codec.unmarshal(wire, &decoded); err != nil {
					t.Fatal(err)
				}
				encoded, err := codec.marshal(decoded)
				if err != nil || string(encoded) != string(wire) {
					t.Fatalf("roundtrip %s: %s, %v", wire, encoded, err)
				}
			}
		})
	}
}

func TestNumberRefusesNonNumbersWithoutReplacingTheOriginalValue(t *testing.T) {
	number, err := value.ParseNumber([]byte("2.50"))
	if err != nil {
		t.Fatal(err)
	}
	for _, wire := range []string{"", `null`, `"1"`, `true`, `{}`, `[]`, `01`, `NaN`} {
		if err := number.UnmarshalJSON([]byte(wire)); err == nil {
			t.Fatalf("accepted %q", wire)
		}
		if number.String() != "2.50" {
			t.Fatalf("failed decode replaced original value: %s", number.String())
		}
	}
	if _, err := json.Marshal(value.Number{}); err == nil {
		t.Fatal("absent number encoded as a value")
	}
}
