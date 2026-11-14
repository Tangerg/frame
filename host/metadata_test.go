package host_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
)

func TestMetadataSnapshotDetachesOverlappingSliceViews(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	point, err := framehost.NewPoint[any, string]("example.metadata", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(point); err != nil {
		t.Fatal(err)
	}
	label := map[string][]string{"label": {"original"}}
	metadata := make([]any, 2)
	metadata[0] = label
	metadata[1] = metadata[:1]
	plugin := installNative(t, host, "metadata", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "entry", Contract: point.Ref()}}},
		Setup: func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) {
			return []framehost.ExportValue{framehost.Contribute("entry", point, any(metadata), "value")}, nil
		},
	})
	if _, err := host.Activate(ctx, source("metadata", plugin, "main", "entry", point.Ref())); err != nil {
		t.Fatalf("finite overlapping metadata was rejected: %v", err)
	}
	snapshot, err := host.Snapshot(ctx, point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := framehost.Candidates(snapshot, point)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("metadata candidates: %v, %v", candidates, err)
	}
	label["label"][0] = "producer mutation"
	firstValue, err := candidates[0].Metadata()
	if err != nil {
		t.Fatal(err)
	}
	first := firstValue.([]any)
	if first[0].(map[string][]string)["label"][0] != "original" || first[1].([]any)[0].(map[string][]string)["label"][0] != "original" {
		t.Fatal("snapshot retained the producer's nested references")
	}
	first[1].([]any)[0].(map[string][]string)["label"][0] = "consumer mutation"
	secondValue, err := candidates[0].Metadata()
	if err != nil {
		t.Fatal(err)
	}
	second := secondValue.([]any)
	if second[1].([]any)[0].(map[string][]string)["label"][0] != "original" {
		t.Fatal("candidate metadata exposed its original nested references")
	}
}

func TestMetadataRefusesCyclesAndLiveValues(t *testing.T) {
	cyclicMap := make(map[string]any)
	cyclicMap["self"] = cyclicMap
	cyclicSlice := make([]any, 1)
	cyclicSlice[0] = cyclicSlice
	for _, test := range []struct {
		name  string
		value any
	}{
		{name: "map cycle", value: cyclicMap},
		{name: "slice cycle", value: cyclicSlice},
		{name: "live callback", value: map[string]any{"callback": func() {}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			host := testHost(t, framehost.Config{})
			point, err := framehost.NewPoint[any, string]("example.invalid-metadata", "1")
			if err != nil {
				t.Fatal(err)
			}
			if err := host.Register(point); err != nil {
				t.Fatal(err)
			}
			plugin := installNative(t, host, "metadata", framehost.Unit{
				Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "entry", Contract: point.Ref()}}},
				Setup: func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) {
					return []framehost.ExportValue{framehost.Contribute("entry", point, test.value, "value")}, nil
				},
			})
			if _, err := host.Activate(testContext(t), source("metadata", plugin, "main", "entry", point.Ref())); !errors.Is(err, frame.ErrInvalid) {
				t.Fatalf("invalid metadata was admitted: %v", err)
			}
		})
	}
}
