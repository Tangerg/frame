package host_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
)

func TestPreparedMetadataKeepsItsOriginalContractAndProjection(t *testing.T) {
	type preparationCase uint8
	const (
		zeroMetadata preparationCase = iota
		differentContract
		newProjection
	)
	for _, scenario := range []struct {
		name string
		kind preparationCase
	}{
		{name: "zero metadata", kind: zeroMetadata},
		{name: "different contract", kind: differentContract},
		{name: "new local projection", kind: newProjection},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			host := testHost(t, framehost.Config{})
			first, err := framehost.NewPoint[string, string]("test.original-metadata", "1")
			if err != nil {
				t.Fatal(err)
			}
			second, err := framehost.NewPoint[string, string]("test.different-metadata", "1")
			if err != nil {
				t.Fatal(err)
			}
			if scenario.kind == newProjection {
				if err := host.RegisterDescriptor(first.Ref(), first.Descriptor()); err != nil {
					t.Fatal(err)
				}
			} else if err := host.Register(first, second); err != nil {
				t.Fatal(err)
			}
			var metadata framehost.PreparedMetadata
			if scenario.kind != zeroMetadata {
				input := []byte(`"original"`)
				if scenario.kind == newProjection {
					input = []byte(`42`)
				}
				metadata, err = host.PrepareMetadata(first.Ref(), input)
				if err != nil {
					t.Fatal(err)
				}
				captured := metadata.JSON().String()
				input[0] = 'x'
				if metadata.JSON().String() != captured {
					t.Fatal("caller rewrote prepared metadata")
				}
			}
			if scenario.kind == newProjection {
				if err := host.Register(first); err != nil {
					t.Fatal(err)
				}
				if _, err := host.PrepareMetadata(first.Ref(), []byte(`42`)); err == nil {
					t.Fatal("new Go projection accepted an invalid value")
				}
			}
			point := first
			if scenario.kind == differentContract {
				point = second
			}
			plugin := installNative(t, host, "metadata", framehost.Unit{
				Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "member", Contract: point.Ref()}}},
				Setup: func(context.Context, *framehost.Setup) ([]framehost.ExportValue, error) {
					return []framehost.ExportValue{{Name: "member", Contract: point.Ref(), Metadata: metadata, Value: "callable"}}, nil
				},
			})
			if _, err := host.Activate(t.Context(), source("metadata", plugin, "main", "member", point.Ref())); !errors.Is(err, frame.ErrContractMismatch) {
				t.Fatalf("prepared metadata changed its admitted contract or projection: %v", err)
			}
		})
	}
}
