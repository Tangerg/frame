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

func TestFeatureConsumerGenerationLeavesPublisherArtifactsUntouched(t *testing.T) {
	directory := t.TempDir()
	t.Chdir(directory)
	method, err := portable.NewMethod[struct{}, string](portable.MethodConfig{Name: "acme/read", Kind: portable.Query, Target: "installation", Description: "Read."})
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
