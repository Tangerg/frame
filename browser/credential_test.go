package browser_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
)

func TestResumeRealmPreservesCredentialReadFailures(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), false)
	t.Cleanup(func() { _ = e.store.Close() })
	credentials, err := e.bridge.OpenRealm(e.ctx, "realm", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	for _, realm := range []browser.Realm{credentials.Realm, {ID: "missing", Epoch: "missing"}} {
		if _, err := e.bridge.ResumeRealm(e.ctx, realm, "wrong-token"); !errors.Is(err, frame.ErrFenced) {
			t.Fatalf("invalid credential classification: %v", err)
		}
	}
	canceled, cancel := context.WithCancel(e.ctx)
	cancel()
	if _, err := e.bridge.ResumeRealm(canceled, credentials.Realm, credentials.ResumeToken); !errors.Is(err, context.Canceled) || errors.Is(err, frame.ErrFenced) {
		t.Fatalf("cancellation cause was lost: %v", err)
	}
	if err := e.store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.bridge.ResumeRealm(e.ctx, credentials.Realm, credentials.ResumeToken); err == nil || errors.Is(err, frame.ErrFenced) {
		t.Fatalf("storage failure was reported as a credential refusal: %v", err)
	}
}
