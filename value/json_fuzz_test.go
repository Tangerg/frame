package value

import "testing"

func FuzzParseJSONIdentityIsStable(f *testing.F) {
	for _, seed := range []string{
		"null", "0", "-0", "1e999999", "[1,2.0,\"x\"]", ` { "b" : 1 , "a" : [ ] } `,
		`{"a":1,"a":2}`, `"\ud800"`, `"é"`, "[", `{"a":{"c":1,"b":2}}`, "\"\xff\"",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		parsed, err := ParseJSON(data)
		if err != nil {
			return
		}
		again, err := ParseJSON([]byte(parsed.String()))
		if err != nil {
			t.Fatalf("admitted document %q does not admit again: %v", parsed, err)
		}
		if again.String() != parsed.String() || again.Digest() != parsed.Digest() {
			t.Fatalf("identity changed on re-admission: %q became %q", parsed, again)
		}
	})
}
