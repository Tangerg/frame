package frame

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Tangerg/frame/value"
)

func TestCapturedPlanRejectsLostAuthorityBeforePublicationWithdrawal(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		revoke func(*Host, ScopeGrant, *unitInstance)
		want   error
	}{
		{"start grant before lifetime cancellation", func(host *Host, grant ScopeGrant, _ *unitInstance) { host.Admission().FenceScope(grant.ID()) }, ErrFenced},
		{"start grant after lifetime cancellation", func(host *Host, grant ScopeGrant, instance *unitInstance) {
			host.Admission().FenceScope(grant.ID())
			instance.lifetime.cancel(ErrFenced)
		}, ErrStaleBinding},
		{"instance lifetime", func(_ *Host, _ ScopeGrant, instance *unitInstance) { instance.lifetime.BeginClose(ErrClosed) }, ErrStaleBinding},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			host, err := NewHost(ctx, HostConfig{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if outcome, err := host.Close(ctx); err != nil || !outcome.Released {
					t.Errorf("host close: %+v, %v", outcome, err)
				}
			}()
			point, err := NewPoint[string, string]("selection.point", "1")
			if err != nil {
				t.Fatal(err)
			}
			if err := host.Register(point); err != nil {
				t.Fatal(err)
			}
			plugin, err := Define(Definition{Revision: value.Hash([]byte("selection-authority")), Units: []Unit{{
				Declaration: UnitDeclaration{ID: "main", Profile: NativeGo, Exports: []ExportDeclaration{{Name: "member", Contract: point.Ref()}}},
				Setup: func(context.Context, *Setup) ([]ExportValue, error) {
					return []ExportValue{Contribute("member", point, "metadata", "value")}, nil
				},
			}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := host.Install(ctx, InstallationConfig{ID: "publisher", Plugin: plugin, Enabled: true}); err != nil {
				t.Fatal(err)
			}
			grant, err := host.Admission().OpenScope(ctx, "publisher.start")
			if err != nil {
				t.Fatal(err)
			}
			running, err := host.StartUnit(ctx, grant, UnitRef{InstallationID: "publisher", Release: plugin.Revision(), UnitID: "main"})
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := host.Snapshot(ctx, point.Ref())
			if err != nil {
				t.Fatal(err)
			}
			candidates, err := Candidates(snapshot, point)
			if err != nil {
				t.Fatal(err)
			}
			selection, err := Select(snapshot, point, "selection", candidates...)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := Capture(snapshot, selection.Ref())
			if err != nil {
				t.Fatal(err)
			}
			// The observer cancels before acquiring Host.mu for withdrawal. Stop it
			// before revocation so both sides of that cancellation are deterministic.
			host.mu.Lock()
			instance := host.allInstances[running.ID()]
			for _, stop := range instance.stops {
				if !stop() {
					host.mu.Unlock()
					t.Fatal("authority observer ran before the controlled revocation")
				}
			}
			scenario.revoke(host, grant, instance)
			err = host.validatePlanLocked(plan)
			host.mu.Unlock()
			if !errors.Is(err, scenario.want) {
				t.Fatalf("captured plan rejection = %v, want %v", err, scenario.want)
			}
		})
	}
}

func TestUseEnterRejectsUnissuedUse(t *testing.T) {
	for _, use := range []*Use{nil, {}} {
		if work, err := use.Enter(t.Context()); !errors.Is(err, ErrInvalid) || work != nil {
			t.Fatalf("unissued use accepted work: %v, %v", work, err)
		}
	}
}
