package frame_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

func TestInstallationRetainsItsCapturedPlugin(t *testing.T) {
	for _, operation := range []string{"install", "update", "replace", "restore"} {
		t.Run(operation, func(t *testing.T) {
			ctx := t.Context()
			host, err := frame.NewHost(ctx, frame.HostConfig{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if outcome, err := host.Close(context.Background()); err != nil || !outcome.Released {
					t.Errorf("host close: %+v, %v", outcome, err)
				}
			})
			contract, err := frame.NewContract[string]("captured.plugin", "1")
			if err != nil {
				t.Fatal(err)
			}
			if err := host.Register(contract); err != nil {
				t.Fatal(err)
			}
			define := func(revision, result string) *frame.Plugin {
				t.Helper()
				plugin, err := frame.Define(frame.Definition{Revision: value.Hash([]byte(revision)), Units: []frame.Unit{{
					Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "result", Contract: contract.Ref()}}},
					Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
						return []frame.ExportValue{frame.Provide("result", contract, result)}, nil
					},
				}}})
				if err != nil {
					t.Fatal(err)
				}
				return plugin
			}
			original := define("original-release", "original implementation")
			expected := frame.InstallationRecord{ID: "captured", Release: original.Revision(), Enabled: true, Units: original.Units()}
			config := frame.InstallationConfig{ID: expected.ID, Plugin: original, Enabled: true}
			switch operation {
			case "install":
				_, err = host.Install(ctx, config)
			case "update", "replace":
				var previous frame.InstallationRecord
				previous, err = host.Install(ctx, frame.InstallationConfig{ID: expected.ID, Plugin: define("previous-release", "previous implementation"), Enabled: true})
				if err == nil {
					if operation == "update" {
						err = host.Update(ctx, previous, config)
					} else {
						err = host.Replace(ctx, expected.ID, original, nil)
					}
				}
			case "restore":
				err = host.Restore(ctx, []frame.InstallationRecord{expected}, []*frame.Plugin{original})
			}
			if err != nil {
				t.Fatal(err)
			}
			*original = *define("replacement-release", "replacement implementation")
			record, exists := host.Installation(expected.ID)
			if !exists || !reflect.DeepEqual(record, expected) {
				t.Fatalf("captured record changed: %+v", record)
			}
			source := frame.ExportRef{InstallationID: record.ID, Release: record.Release, UnitID: "main", Export: "result", Contract: contract.Ref()}
			plan, err := host.Plan(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			scope, err := host.Admission().OpenScope(ctx, "capture.consumer")
			if err != nil {
				t.Fatal(err)
			}
			if err := frame.WithPlan(ctx, host, plan, scope, func(use *frame.Use) error {
				return frame.BorrowExport(ctx, use, source, func(_ context.Context, got string) error {
					if got != "original implementation" {
						t.Errorf("captured release executed %q", got)
					}
					return nil
				})
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
