package contractgen

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame/portable"
)

func TestGeneratedNullableOutputAndChunkUseTheDeclaredCodec(t *testing.T) {
	descriptor, err := featureDescriptor(portable.MethodDescriptor{
		Name: "read", Kind: portable.Query, Description: "Read a nullable value.",
		Input:  []byte(`{"type":"object","additionalProperties":false}`),
		Output: []byte(`{"type":["string","null"]}`),
		Stream: []byte(`{"type":["string","null"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	source := filepath.Join(directory, "capability.json")
	if err := os.WriteFile(source, descriptor, 0600); err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(directory, "types.go")
	if err := Generate(source, "", directory, GoOutput{Path: generated, Package: "consumer"}); err != nil {
		t.Fatal(err)
	}
	consumer := `package consumer
import (
    "testing"
    "github.com/Tangerg/frame/portable"
    "github.com/Tangerg/frame/value"
)
func TestNullableFrames(t *testing.T) {
    capability, err := portable.ParseCapability([]byte(CapabilityJSON))
    if err != nil { t.Fatal(err) }
    method, found := capability.Method(ReadMethod)
    if !found { t.Fatal("generated method is absent") }
    for _, wire := range []string{"null", "\"kept\""} {
        if err := method.ValidateOutput([]byte(wire)); err != nil { t.Fatal(err) }
        if err := method.ValidateChunk([]byte(wire)); err != nil { t.Fatal(err) }
        for _, destination := range []any{new(ReadOutput), new(ReadChunk)} {
            if err := value.DecodeValue([]byte(wire), destination, "result"); err != nil { t.Fatal(err) }
            encoded, err := value.EncodeJSON(destination)
            if err != nil || encoded.String() != wire { t.Fatalf("changed %s into %s: %v", wire, encoded, err) }
        }
    }
    if method.ValidateOutput([]byte("7")) == nil || method.ValidateChunk([]byte("7")) == nil {
        t.Fatal("opaque Go representation widened the canonical predicate")
    }
}
`
	test := filepath.Join(directory, "consumer_test.go")
	if err := os.WriteFile(test, []byte(consumer), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "go", "test", generated, test)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated nullable consumer: %s\n%v", output, err)
	}
	typescript := `import type {ReadOutput, ReadChunk} from "./methods.generated.js";
const output: ReadOutput = null;
const chunk: ReadChunk = null;
const present: ReadOutput = "kept";
// @ts-expect-error The descriptor excludes numeric output.
const invalidOutput: ReadOutput = 7;
// @ts-expect-error The descriptor excludes numeric chunks.
const invalidChunk: ReadChunk = 7;
void [output, chunk, present, invalidOutput, invalidChunk];
`
	if err := os.WriteFile(filepath.Join(directory, "consumer.ts"), []byte(typescript), 0600); err != nil {
		t.Fatal(err)
	}
	runTypeScriptConsumer(t, directory)
}
