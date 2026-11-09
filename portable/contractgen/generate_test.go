package contractgen

import (
	json "encoding/json/v2"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame/portable"
)

func TestFeatureConsumerGenerationAcceptsAnUndocumentedMethod(t *testing.T) {
	directory := t.TempDir()
	t.Chdir(directory)
	method, err := portable.NewMethod[struct{}, string](portable.MethodConfig{Name: "acme/read", Kind: portable.Query, Target: "installation"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := featureDescriptor(method.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(directory, "capability.generated.json")
	if err := os.WriteFile(source, data, 0644); err != nil {
		t.Fatal(err)
	}
	projection := filepath.Join(directory, "consumer", "contract.generated.go")
	if err := Generate(source, "", "", GoOutput{Path: projection, Package: "consumer"}); err != nil {
		t.Fatal(err)
	}
	retained, err := os.ReadFile(source)
	if err != nil || string(retained) != string(data) {
		t.Fatalf("publisher descriptor changed: %v", err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), projection, nil, parser.AllErrors); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 2 {
		t.Fatalf("consumer generated publisher artifacts: %v, %v", entries, err)
	}
}

func TestFeatureCanonicalDescriptorCanGenerateConsumers(t *testing.T) {
	method, err := portable.NewMethod[struct{}, string](portable.MethodConfig{Name: "acme/read", Kind: portable.Query, Description: "Read the retained value."})
	if err != nil {
		t.Fatal(err)
	}
	data, err := featureDescriptor(method.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	capability, err := portable.ParseCapability(data)
	if err != nil {
		t.Fatal(err)
	}
	canonical := capability.Identity().Bytes()
	if _, err := portable.ParseCapability(canonical); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	source := filepath.Join(directory, "capability.json")
	if err := os.WriteFile(source, canonical, 0600); err != nil {
		t.Fatal(err)
	}
	if err := Generate(source, "", directory, GoOutput{Path: filepath.Join(directory, "types.go"), Package: "consumer"}); err != nil {
		t.Fatal(err)
	}
}

func TestFeatureGenerationCannotOverwriteDescriptorSource(t *testing.T) {
	for _, name := range []string{"methods.generated.json", "capability.generated.json"} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			source := filepath.Join(directory, name)
			data := []byte(`{"authored":"retained"}`)
			if err := os.WriteFile(source, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := Generate(source, directory, ""); err == nil {
				t.Fatal("generator overwrote its descriptor input")
			}
			retained, err := os.ReadFile(source)
			if err != nil || string(retained) != string(data) {
				t.Fatalf("source changed: %s, %v", retained, err)
			}
		})
	}
}

func featureDescriptor(methods ...portable.MethodDescriptor) ([]byte, error) {
	return json.Marshal(portable.CapabilityDescriptor{Protocol: portable.Protocol, Reference: "acme/reviews", Methods: methods})
}

func TestFeatureGenerationRefusesSourceHardLinkBeforeWritingAnyProjection(t *testing.T) {
	method, err := portable.NewMethod[struct{}, string](portable.MethodConfig{Name: "acme/read", Kind: portable.Query, Target: "installation", Description: "Read."})
	if err != nil {
		t.Fatal(err)
	}
	data, err := featureDescriptor(method.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	source := filepath.Join(directory, "source.json")
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(directory, "aliased.go")
	if err := os.Link(source, target); err != nil {
		t.Fatal(err)
	}
	outputs := filepath.Join(directory, "projections")
	if err := Generate(source, outputs, "", GoOutput{Path: target, Package: "consumer"}); err == nil {
		t.Fatal("overwrote source through hard link")
	}
	retained, err := os.ReadFile(source)
	if err != nil || string(retained) != string(data) {
		t.Fatalf("source changed: %v", err)
	}
	if _, err := os.Stat(outputs); !os.IsNotExist(err) {
		t.Fatalf("wrote outputs before admitting all targets: %v", err)
	}
}

func TestPublicationAdmitsEveryPhysicalDestinationBeforeReplacingFiles(t *testing.T) {
	directory := t.TempDir()
	first := filepath.Join(directory, "first.go")
	initial := []byte("original contents")
	if err := os.WriteFile(first, initial, 0600); err != nil {
		t.Fatal(err)
	}
	hardlink := filepath.Join(directory, "hardlink.go")
	if err := os.Link(first, hardlink); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(directory, "alias")
	if err := os.Symlink(directory, alias); err != nil {
		t.Fatal(err)
	}
	for _, outputs := range [][]artifact{
		{{first, []byte("new")}, {hardlink, []byte("different")}},
		{{filepath.Join(directory, "new", "out.go"), []byte("first")}, {filepath.Join(alias, "new", "out.go"), []byte("second")}},
		{{first, []byte("new")}, {directory, []byte("cannot replace directory")}},
		{{first, []byte("new")}, {filepath.Join(first, "child.go"), []byte("invalid descendant")}},
	} {
		if err := publishArtifacts("-", outputs); err == nil {
			t.Fatal("admitted colliding or unwritable targets")
		}
		content, err := os.ReadFile(first)
		if err != nil || string(content) != string(initial) {
			t.Fatalf("altered earlier projection: %s, %v", content, err)
		}
	}
	if err := os.Remove(hardlink); err != nil {
		t.Fatal(err)
	}
	if err := publishArtifacts("-", []artifact{{first, []byte("new")}, {hardlink, []byte("separate")}}); err != nil {
		t.Fatal(err)
	}
}

func TestCommitFailureRestoresPreviouslyPublishedProjections(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "first.go")
	if err := os.WriteFile(target, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	stages := []stagedArtifact{{target: target, original: original}, {target: filepath.Join(directory, "second.go")}}
	for i := range stages {
		if err := stages[i].prepare([]byte("next")); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(stages[1].stage, "next")); err != nil {
		t.Fatal(err)
	}
	if err := commitArtifacts(stages); err == nil {
		t.Fatal("missing projection committed")
	}
	restored, err := os.Stat(target)
	if err != nil || !os.SameFile(original, restored) {
		t.Fatalf("lost original file on failed publication: %v", err)
	}
}
