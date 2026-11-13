package host_test

import (
	"context"
	"reflect"
	"testing"

	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

func TestInstallationRetainsItsCapturedPlugin(t *testing.T) {
	for _, operation := range []string{"install", "update", "replace", "restore"} {
		t.Run(operation, func(t *testing.T) {
			ctx := t.Context()
			host, err := framehost.New(ctx, framehost.Config{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if outcome, err := host.Close(context.Background()); err != nil || !outcome.Released {
					t.Errorf("host close: %+v, %v", outcome, err)
				}
			})
			contract, err := framehost.NewContract[string]("captured.plugin", "1")
			if err != nil {
				t.Fatal(err)
			}
			if err := host.Register(contract); err != nil {
				t.Fatal(err)
			}
			define := func(revision, result string) *framehost.Plugin {
				t.Helper()
				plugin, err := framehost.Define(framehost.Definition{Revision: value.Hash([]byte(revision)), Units: []framehost.Unit{{
					Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "result", Contract: contract.Ref()}}},
					Setup: func(context.Context, *framehost.Setup) ([]framehost.ExportValue, error) {
						return []framehost.ExportValue{framehost.Provide("result", contract, result)}, nil
					},
				}}})
				if err != nil {
					t.Fatal(err)
				}
				return plugin
			}
			original := define("original-release", "original implementation")
			expected := framehost.InstallationRecord{ID: "captured", Release: original.Revision(), Enabled: true, Units: original.Units()}
			config := framehost.InstallationConfig{ID: expected.ID, Plugin: original, Enabled: true}
			switch operation {
			case "install":
				_, err = host.Install(ctx, config)
			case "update", "replace":
				var previous framehost.InstallationRecord
				previous, err = host.Install(ctx, framehost.InstallationConfig{ID: expected.ID, Plugin: define("previous-release", "previous implementation"), Enabled: true})
				if err == nil {
					if operation == "update" {
						err = host.Update(ctx, previous, config)
					} else {
						err = host.Replace(ctx, expected.ID, original, nil)
					}
				}
			case "restore":
				err = host.Restore(ctx, []framehost.InstallationRecord{expected}, []*framehost.Plugin{original})
			}
			if err != nil {
				t.Fatal(err)
			}
			*original = *define("replacement-release", "replacement implementation")
			record, exists := host.Installation(expected.ID)
			if !exists || !reflect.DeepEqual(record, expected) {
				t.Fatalf("captured record changed: %+v", record)
			}
			source := framehost.ExportRef{InstallationID: record.ID, Release: record.Release, UnitID: "main", Export: "result", Contract: contract.Ref()}
			plan, err := host.Plan(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			scope, err := host.Admission().OpenScope(ctx, "capture.consumer")
			if err != nil {
				t.Fatal(err)
			}
			if err := framehost.WithPlan(ctx, host, plan, scope, func(use *framehost.Use) error {
				return framehost.BorrowExport(ctx, use, source, func(_ context.Context, got string) error {
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
