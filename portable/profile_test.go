package portable_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/value"
)

func TestSharedSchemaProfileCorpus(t *testing.T) {
	data, err := os.ReadFile("testdata/schema-profile.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name     string          `json:"name"`
		Schema   json.RawMessage `json:"schema"`
		Admitted bool            `json:"admitted"`
		Digest   string          `json:"digest"`
		Values   []struct {
			Value json.RawMessage `json:"value"`
			Valid bool            `json:"valid"`
		} `json:"values"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, test := range cases {
		t.Run(test.Name, func(t *testing.T) {
			document, err := value.ParseJSON(test.Schema)
			if err != nil || document.Digest().String() != test.Digest {
				t.Fatalf("canonical schema identity changed: %v", err)
			}
			schema, err := portable.AdmitSchema(test.Schema)
			if (err == nil) != test.Admitted {
				t.Fatalf("admission=%v: %v", test.Admitted, err)
			}
			if err != nil {
				return
			}
			for _, sample := range test.Values {
				if err := schema.Validate(sample.Value); (err == nil) != sample.Valid {
					t.Errorf("%s: valid=%v: %v", sample.Value, sample.Valid, err)
				}
			}
		})
	}
}
