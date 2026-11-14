package browser_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame/browser"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/sqlite"
)

func TestBridgeRequiresTheOriginalDurableAuthority(t *testing.T) {
	gate := framehost.NewAdmission()
	a, err := sqlite.Open(t.Context(), sqlite.Config{Path: filepath.Join(t.TempDir(), "a.db"), Admission: gate})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := sqlite.Open(t.Context(), sqlite.Config{Path: filepath.Join(t.TempDir(), "b.db"), Admission: gate})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	host, err := framehost.New(t.Context(), framehost.Config{Authority: a})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(t.Context())
	if _, err = browser.NewBridge(browser.BridgeConfig{Host: host, Store: b, Source: testModules{}}); err == nil {
		t.Fatal("substituted an unrelated store at the same epoch")
	}
}

func TestStoreHandleReplacementCannotRetargetTheHostAuthority(t *testing.T) {
	gate := framehost.NewAdmission()
	original, err := sqlite.Open(t.Context(), sqlite.Config{Path: filepath.Join(t.TempDir(), "original.db"), Admission: gate})
	if err != nil {
		t.Fatal(err)
	}
	copied := original
	defer copied.Close()
	replacement, err := sqlite.Open(t.Context(), sqlite.Config{Path: filepath.Join(t.TempDir(), "replacement.db"), Admission: gate})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	host, err := framehost.New(t.Context(), framehost.Config{Authority: original})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(t.Context())
	bridge, err := browser.NewBridge(browser.BridgeConfig{Host: host, Store: original, Source: testModules{}})
	if err != nil {
		t.Fatal(err)
	}
	original = replacement
	if host.Durable() == framehost.Durable(original) {
		t.Fatal("replacing a caller-owned Store retargeted the Host authority")
	}
	if host.Durable() != framehost.Durable(copied) {
		t.Fatal("copy no longer denotes the Host's original durable authority")
	}
	if _, err := browser.NewBridge(browser.BridgeConfig{Host: host, Store: copied, Source: testModules{}}); err != nil {
		t.Fatalf("original authority cannot compose its browser driver: %v", err)
	}
	credentials, err := bridge.OpenRealm(t.Context(), "open-original", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.SealRealm(t.Context(), credentials.Token)
	if credential, err := copied.RealmOperation(t.Context(), "open-original"); err != nil || credential.ID != credentials.Realm.ID {
		t.Fatalf("original driver lost its database: %+v, %v", credential, err)
	}
	if _, err := replacement.RealmOperation(t.Context(), "open-original"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("original driver wrote to the replacement database: %v", err)
	}
}
