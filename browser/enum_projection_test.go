package browser

import (
	json "encoding/json/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type publishedPhase string

func (publishedPhase) JSONSchemaEnum() []string { return []string{"draft", "ready"} }

func TestTypeScriptUsesThePublishedEnumVocabulary(t *testing.T) {
	var out strings.Builder
	if err := WriteTypeScriptTypes(&out, reflect.TypeFor[publishedPhase]()); err != nil {
		t.Fatal(err)
	}
	checkTypeScriptConsumer(t, out.String(), `
import type { publishedPhase } from "./types.generated.js";
const admitted: publishedPhase = "ready";
// @ts-expect-error The published enum is closed over its owner's vocabulary.
const invalid: publishedPhase = "invented";
void [admitted, invalid];
`)
}

type publishedNarrowedPhase struct{ value string }

func (publishedNarrowedPhase) JSONSchemaModel() any           { return publishedPhase("") }
func (publishedNarrowedPhase) JSONSchemaEnum() []string       { return []string{"ready", "closed"} }
func (v publishedNarrowedPhase) MarshalJSON() ([]byte, error) { return json.Marshal(v.value) }
func (v *publishedNarrowedPhase) UnmarshalJSON(raw []byte) error {
	return json.Unmarshal(raw, &v.value)
}

type publishedWrappedPhase struct{ inner publishedNarrowedPhase }

func (publishedWrappedPhase) JSONSchemaModel() any           { return publishedNarrowedPhase{} }
func (v publishedWrappedPhase) MarshalJSON() ([]byte, error) { return json.Marshal(v.inner.value) }
func (v *publishedWrappedPhase) UnmarshalJSON(raw []byte) error {
	return json.Unmarshal(raw, &v.inner.value)
}

func TestGeneratedConsumerKeepsComposedEnumConstraints(t *testing.T) {
	var out strings.Builder
	if err := WriteTypeScriptHTTP(&out, []HTTPOperation{{Name: "phase", Method: http.MethodGet, Path: "/phase", SuccessStatus: http.StatusOK, Output: reflect.TypeFor[publishedWrappedPhase]()}}); err != nil {
		t.Fatal(err)
	}
	checkTypeScriptConsumer(t, out.String(), `
import type {publishedWrappedPhase} from "./types.generated.js";
const admitted: publishedWrappedPhase = "ready";
// @ts-expect-error The codec cannot expand its model's vocabulary.
const draft: publishedWrappedPhase = "draft";
// @ts-expect-error The model cannot expand its codec's vocabulary.
const closed: publishedWrappedPhase = "closed";
void [admitted, draft, closed];
`)
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "types.generated.ts"), []byte(out.String()), 0600); err != nil {
		t.Fatal(err)
	}
	const consumer = `import assert from "node:assert/strict";
import {decodeHTTPOutput} from "./types.generated.ts";
assert.equal(decodeHTTPOutput("phase", "ready"), "ready");
for (const invalid of ["draft", "closed", "invented", null, 42])
  assert.throws(() => decodeHTTPOutput("phase", invalid), TypeError);
`
	path := filepath.Join(directory, "consumer.mjs")
	if err := os.WriteFile(path, []byte(consumer), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(t.Context(), "node", path).CombinedOutput(); err != nil {
		t.Fatalf("generated enum admission changed its model: %s, %v", output, err)
	}
}
