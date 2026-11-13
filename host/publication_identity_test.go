package host_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
)

func TestSetupDeclarationOwnsExportReconstruction(t *testing.T) {
	host := testHost(t, framehost.Config{})
	service, err := framehost.NewContract[string]("example.reconstruction.service", "1")
	if err != nil {
		t.Fatal(err)
	}
	point, err := framehost.NewPoint[string, string]("example.reconstruction.point", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(service, point); err != nil {
		t.Fatal(err)
	}
	plugin := installNative(t, host, "provider", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "service", Contract: service.Ref()}, {Name: "declared", Contract: point.Ref()}}},
		Setup: func(context.Context, *framehost.Setup) ([]framehost.ExportValue, error) {
			return []framehost.ExportValue{
				{Name: "service", Contract: service.Ref(), Value: "service value"},
				{Name: "declared", Contract: point.Ref(), Metadata: "declared metadata", Value: "declared value"},
				{Name: "dynamic", Contract: point.Ref(), Metadata: "dynamic metadata", Value: "dynamic value"},
			}, nil
		},
	})
	if _, err := host.ActivateUnit(t.Context(), "", framehost.UnitRef{InstallationID: "provider", Release: plugin.Revision(), UnitID: "main"}); err != nil {
		t.Fatal(err)
	}
	owner, scope := ownerAndScope(t, host)
	config := framehost.UseConfig{Owner: owner, Scope: scope}
	for _, test := range []struct {
		name     string
		contract framehost.ContractRef
		declared bool
	}{
		{name: "service", contract: service.Ref(), declared: true},
		{name: "declared", contract: point.Ref(), declared: true},
		{name: "dynamic", contract: point.Ref()},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := source("provider", plugin, "main", test.name, test.contract)
			if test.declared {
				plan, err := host.Plan(t.Context(), root)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := plan.Snapshot(); err != nil {
					t.Fatalf("declared plan cannot reconstruct its slot: %v", err)
				}
				use, err := host.Acquire(t.Context(), plan, config)
				if err != nil {
					t.Fatalf("exact acquisition lost the declared slot: %v", err)
				}
				if _, err := use.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, err := host.Snapshot(t.Context(), test.contract)
			if err != nil {
				t.Fatal(err)
			}
			candidates, err := snapshot.Candidates(test.contract)
			if err != nil {
				t.Fatal(err)
			}
			var selected []framehost.CandidateRef
			for _, candidate := range candidates {
				if candidate.Source() == root {
					selected = append(selected, candidate)
				}
			}
			if len(selected) != 1 {
				t.Fatalf("expected one original publication, got %d", len(selected))
			}
			selection, err := framehost.SelectRefs(snapshot, test.contract, "original", selected...)
			if err != nil {
				t.Fatal(err)
			}
			captured, err := framehost.Capture(snapshot, selection)
			if err != nil {
				t.Fatal(err)
			}
			_, err = captured.Snapshot()
			if test.declared && err != nil || !test.declared && !errors.Is(err, frame.ErrInvalid) {
				t.Fatalf("captured reconstruction disagrees with declaration=%t: %v", test.declared, err)
			}
			use, err := host.Acquire(t.Context(), captured, config)
			if err != nil {
				t.Fatalf("captured acquisition: %v", err)
			}
			if err := framehost.BorrowExport(t.Context(), use, root, func(_ context.Context, value string) error {
				if value != test.name+" value" {
					t.Errorf("borrowed another publication: %q", value)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := use.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
