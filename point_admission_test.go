package frame_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
)

func TestMultiPointCaptureValidatesItsWholeReadSet(t *testing.T) {
	for _, change := range []string{"selected member", "unselected member", "empty point", "metadata", "unrelated point"} {
		t.Run(change, func(t *testing.T) {
			ctx := testContext(t)
			host := testHost(t, frame.HostConfig{})
			first, _ := frame.NewPoint[string, string]("example.first", "1")
			second, _ := frame.NewPoint[string, string]("example.second", "1")
			empty, _ := frame.NewPoint[string, string]("example.empty", "1")
			unrelated, _ := frame.NewPoint[string, string]("example.unrelated", "1")
			if err := host.Register(first, second, empty, unrelated); err != nil {
				t.Fatal(err)
			}
			var instanceID string
			plugin := installNative(t, host, "points", frame.Unit{
				Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{
					{Name: "first", Contract: first.Ref()},
					{Name: "second", Contract: second.Ref()},
					{Name: "empty-seed", Contract: empty.Ref()},
					{Name: "unrelated-seed", Contract: unrelated.Ref()},
				}},
				Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
					instanceID = setup.InstanceID()
					return []frame.ExportValue{
						frame.Contribute("first", first, "selected", "first-value"),
						frame.Contribute("other", first, "not selected", "unused-value"),
						frame.Contribute("second", second, "selected", "second-value"),
						frame.Contribute("empty-seed", empty, "seed", "empty-value"),
						frame.Contribute("unrelated-seed", unrelated, "seed", "unrelated-value"),
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
			seed, err := frame.Candidates(seedSnapshot, empty)
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
			firstCandidates, err := frame.Candidates(snapshot, first)
			if err != nil || len(firstCandidates) != 2 {
				t.Fatalf("first Point: %+v, %v", firstCandidates, err)
			}
			secondCandidates, err := frame.Candidates(snapshot, second)
			if err != nil || len(secondCandidates) != 1 {
				t.Fatalf("second Point: %+v, %v", secondCandidates, err)
			}
			firstSelection, err := frame.Select(snapshot, first, "first@1", firstCandidates[0])
			if err != nil {
				t.Fatal(err)
			}
			secondSelection, err := frame.Select(snapshot, second, "second@1", secondCandidates[0])
			if err != nil {
				t.Fatal(err)
			}
			emptySelection, err := frame.Select(snapshot, empty, "none@1")
			if err != nil {
				t.Fatal(err)
			}
			plan, err := frame.Capture(snapshot, firstSelection.Ref(), secondSelection.Ref(), emptySelection.Ref())
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "selected member":
				err = host.Withdraw(ctx, secondCandidates[0].ID())
			case "unselected member":
				err = host.Withdraw(ctx, firstCandidates[1].ID())
			case "empty point":
				_, err = host.Publish(ctx, "empty-added", preparePublication(t, host, instanceID, frame.Contribute("new-empty-member", empty, "new", "new-value")))
			case "metadata":
				// Membership metadata is immutable. A revision publishes a new
				// member under the same logical source after withdrawing the old.
				if err := host.Withdraw(ctx, firstCandidates[1].ID()); err != nil {
					t.Fatal(err)
				}
				_, err = host.Publish(ctx, "metadata-revised", preparePublication(t, host, instanceID, frame.Contribute("other", first, "now preferred", "unused-value")))
			case "unrelated point":
				_, err = host.Publish(ctx, "unrelated-added", preparePublication(t, host, instanceID, frame.Contribute("another-unrelated", unrelated, "new", "new-value")))
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
			useErr := frame.WithPlan(ctx, host, plan, scope, func(use *frame.Use) error {
				calls++
				if err := frame.Borrow(ctx, use, firstSelection, func(_ context.Context, values []string) error {
					if len(values) != 1 || values[0] != "first-value" {
						t.Fatalf("first composition value: %+v", values)
					}
					return nil
				}); err != nil {
					return err
				}
				return frame.Borrow(ctx, use, secondSelection, func(_ context.Context, values []string) error {
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
			} else if !errors.Is(commitErr, frame.ErrStaleBinding) || !errors.Is(useErr, frame.ErrStaleBinding) || committed || calls != 0 {
				t.Fatalf("part of a stale composition was admitted: commit=%v, use=%v, committed=%v, calls=%d", commitErr, useErr, committed, calls)
			}
		})
	}
}
