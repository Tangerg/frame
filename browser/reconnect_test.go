package browser_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
)

func TestReconnectTransientFailurePreservesRealm(t *testing.T) {
	for _, mode := range []string{"canceled", "recovery"} {
		t.Run(mode, func(t *testing.T) {
			e := openBridge(t, filepath.Join(t.TempDir(), "reconnect.db"), true)
			plan := e.start("component")
			request := browser.ReconnectRequest{Authority: plan.Authority, Realm: plan.Realm, Executions: []browser.Execution{plan.Execution}}
			if result, err := e.bridge.Reconnect(e.ctx, e.credentials.Token, request); err != nil || result.Mode != browser.Revalidated {
				t.Fatalf("positive control: %+v %v", result, err)
			}
			ctx := e.ctx
			var expected error
			switch mode {
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(e.ctx)
				cancel()
				expected = context.Canceled
			case "recovery":
				expected = frame.ErrRecoveryRequired
				e.host.Admission().RequireRecovery(frame.ErrFenced)
			}
			result, err := e.bridge.Reconnect(ctx, e.credentials.Token, request)
			if !errors.Is(err, expected) || result.Mode == browser.CleanupOnly {
				t.Fatalf("unavailable validation changed realm authority: %+v, %v", result, err)
			}
			if mode == "recovery" {
				if err := e.host.Admission().Recover(e.ctx, func() error { return nil }); err != nil {
					t.Fatal(err)
				}
			}
			result, err = e.bridge.Reconnect(e.ctx, e.credentials.Token, request)
			_, snapshotErr := e.bridge.Snapshot(e.ctx, e.credentials.Token, browser.SnapshotRequest{OperationID: "after-transient", Points: []string{e.first.Ref().Name}})
			if err != nil || result.Mode != browser.Revalidated || snapshotErr != nil {
				t.Fatalf("transient %s validation permanently sealed original realm: retry=%+v err=%v snapshot=%v", mode, result, err, snapshotErr)
			}
			if err := e.bridge.CloseSnapshot(e.ctx, e.credentials.Token, "after-transient"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReconnectStaleAuthoritySealsRealm(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "stale.db"), true)
	plan := e.start("component")
	authority := plan.Authority
	authority.Epoch = "previous-epoch"
	result, err := e.bridge.Reconnect(e.ctx, e.credentials.Token, browser.ReconnectRequest{Authority: authority, Realm: plan.Realm, Executions: []browser.Execution{plan.Execution}})
	if err != nil || result.Mode != browser.CleanupOnly {
		t.Fatalf("stale authority was not fenced: result=%+v err=%v", result, err)
	}
	_, err = e.bridge.Snapshot(e.ctx, e.credentials.Token, browser.SnapshotRequest{OperationID: "after-stale", Points: []string{e.first.Ref().Name}})
	if !errors.Is(err, frame.ErrFenced) {
		t.Fatalf("stale authority kept the realm live: %v", err)
	}
}
