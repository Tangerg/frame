package portable

import "testing"

func FuzzAdmitSchemaIsDeterministic(f *testing.F) {
	for _, seed := range []string{
		"{}", "true", `{"type":"object","properties":{"at":{"type":"string","format":"date-time"}},"required":["at"]}`,
		`{"$ref":"#"}`, `{"pattern":"("}`, `{"minimum":1e400}`, `{"type":"array","uniqueItems":true,"items":{"type":"integer"}}`,
		`{"$defs":{"a":{"$ref":"#/$defs/a"}},"$ref":"#/$defs/a"}`, `{"enum":[1,1.0]}`, "[",
	} {
		f.Add([]byte(seed))
	}
	instances := [][]byte{[]byte("null"), []byte("0"), []byte(`""`), []byte("{}"), []byte("[1,1]"), []byte(`{"at":"2026-11-05T00:00:00Z"}`)}
	f.Fuzz(func(t *testing.T, raw []byte) {
		schema, err := AdmitSchema(raw)
		again, againErr := AdmitSchema(raw)
		if (err == nil) != (againErr == nil) {
			t.Fatalf("admission of %q is not deterministic: %v then %v", raw, err, againErr)
		}
		if err != nil {
			return
		}
		for _, instance := range instances {
			if (schema.Validate(instance) == nil) != (again.Validate(instance) == nil) {
				t.Fatalf("validation of %s against %q is not deterministic", instance, raw)
			}
		}
	})
}
