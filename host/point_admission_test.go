package host_test

import (
	"context"
	"errors"
	"testing"

	framehost "github.com/Tangerg/frame/host"
)

func TestMultiPointCaptureValidatesItsWholeReadSet(t *testing.T) {
	for _, change := range []string{"selected member", "unselected member", "empty point", "metadata", "unrelated point"} {
		t.Run(change, func(t *testing.T) {
			ctx := testContext(t)
			host := testHost(t, framehost.Config{})
			first, _ := framehost.NewPoint[string, string]("example.first", "1")
			second, _ := framehost.NewPoint[string, string]("example.second", "1")
			empty, _ := framehost.NewPoint[string, string]("example.empty", "1")
			unrelated, _ := framehost.NewPoint[string, string]("example.unrelated", "1")
			if err := host.Register(first, second, empty, unrelated); err != nil {
				t.Fatal(err)
			}
			var instanceID string
			plugin := installNative(t, host, "points", framehost.Unit{
				Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{
					{Name: "first", Contract: first.Ref()},
					{Name: "second", Contract: second.Ref()},
					{Name: "empty-seed", Contract: empty.Ref()},
					{Name: "unrelated-seed", Contract: unrelated.Ref()},
				}},
				Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
					instanceID = setup.InstanceID()
					return []framehost.ExportValue{
						framehost.Contribute("first", first, "selected", "first-value"),
						framehost.Contribute("other", first, "not selected", "unused-value"),
						framehost.Contribute("second", second, "selected", "second-value"),
						framehost.Contribute("empty-seed", empty, "seed", "empty-value"),
						framehost.Contribute("unrelated-seed", unrelated, "seed", "unrelated-value"),
					}, nil
				},
			})
			if _, err := host.Activate(ctx, source("points", plugin, "main", "first", first.Ref())); err != nil {
				t.Fatal(err)
			}
			seedSnapshot, err := host.Snapshot(ctx, empty.Ref())
			if err != nil {
				t.Fatal(err)
			}
			seed, err := framehost.Candidates(seedSnapshot, empty)
			if err != nil || len(seed) != 1 {
				t.Fatalf("empty Point seed: %+v, %v", seed, err)
			}
			if err := host.Withdraw(ctx, seed[0].ID()); err != nil {
				t.Fatal(err)
			}
			snapshot, err := host.Snapshot(ctx, first.Ref(), second.Ref(), empty.Ref())
			if err != nil {
				t.Fatal(err)
			}
			firstCandidates, err := framehost.Candidates(snapshot, first)
			if err != nil || len(firstCandidates) != 2 {
				t.Fatalf("first Point: %+v, %v", firstCandidates, err)
			}
			secondCandidates, err := framehost.Candidates(snapshot, second)
			if err != nil || len(secondCandidates) != 1 {
				t.Fatalf("second Point: %+v, %v", secondCandidates, err)
			}
			firstSelection, err := framehost.Select(snapshot, first, "first@1", firstCandidates[0])
			if err != nil {
				t.Fatal(err)
			}
			secondSelection, err := framehost.Select(snapshot, second, "second@1", secondCandidates[0])
			if err != nil {
				t.Fatal(err)
			}
			emptySelection, err := framehost.Select(snapshot, empty, "none@1")
			if err != nil {
				t.Fatal(err)
			}
			plan, err := framehost.Capture(snapshot, firstSelection.Ref(), secondSelection.Ref(), emptySelection.Ref())
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "selected member":
				err = host.Withdraw(ctx, secondCandidates[0].ID())
			case "unselected member":
				err = host.Withdraw(ctx, firstCandidates[1].ID())
			case "empty point":
				_, err = host.Publish(ctx, "empty-added", preparePublication(t, host, instanceID, framehost.Contribute("new-empty-member", empty, "new", "new-value")))
			case "metadata":
				// Membership metadata is immutable. A revision publishes a new
				// member under the same logical source after withdrawing the old.
				if err := host.Withdraw(ctx, firstCandidates[1].ID()); err != nil {
					t.Fatal(err)
				}
				_, err = host.Publish(ctx, "metadata-revised", preparePublication(t, host, instanceID, framehost.Contribute("other", first, "now preferred", "unused-value")))
			case "unrelated point":
				_, err = host.Publish(ctx, "unrelated-added", preparePublication(t, host, instanceID, framehost.Contribute("another-unrelated", unrelated, "new", "new-value")))
			}
			if err != nil {
				t.Fatal(err)
			}
			committed := false
			commitErr := host.Admission().Admit(ctx, func(admitted context.Context) error {
				if _, err := host.ValidatePlanUnder(admitted, plan); err != nil {
					return err
				}
				committed = true
				return nil
			})
			scope, err := host.Admission().OpenScope(ctx, "whole-composition")
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			useErr := framehost.WithPlan(ctx, host, plan, scope, func(use *framehost.Use) error {
				calls++
				if err := framehost.Borrow(ctx, use, firstSelection, func(_ context.Context, values []string) error {
					if len(values) != 1 || values[0] != "first-value" {
						t.Fatalf("first composition value: %+v", values)
					}
					return nil
				}); err != nil {
					return err
				}
				return framehost.Borrow(ctx, use, secondSelection, func(_ context.Context, values []string) error {
					if len(values) != 1 || values[0] != "second-value" {
						t.Fatalf("second composition value: %+v", values)
					}
					return nil
				})
			})
			if change == "unrelated point" {
				if commitErr != nil || useErr != nil || !committed || calls != 1 {
					t.Fatalf("unread Point invalidated composition: commit=%v, use=%v, committed=%v, calls=%d", commitErr, useErr, committed, calls)
				}
			} else if !errors.Is(commitErr, framehost.ErrStaleBinding) || !errors.Is(useErr, framehost.ErrStaleBinding) || committed || calls != 0 {
				t.Fatalf("part of a stale composition was admitted: commit=%v, use=%v, committed=%v, calls=%d", commitErr, useErr, committed, calls)
			}
		})
	}
}
