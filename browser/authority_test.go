package browser_test

import (
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
	"github.com/Tangerg/frame/sqlite"
)

func TestBridgeRequiresTheOriginalDurableAuthority(t *testing.T) {
	gate := frame.NewAdmission()
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
	host, err := frame.NewHost(t.Context(), frame.HostConfig{Admission: gate, Epoch: a.Epoch(), Installations: a, ExecutionProtection: a})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(t.Context())
	if _, err = browser.NewBridge(browser.BridgeConfig{Host: host, Store: b, Source: testModules{}}); err == nil {
		t.Fatal("substituted an unrelated store at the same epoch")
	}
}
