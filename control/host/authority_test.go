package host_test

import (
	"github.com/Tangerg/frame"
	controlhost "github.com/Tangerg/frame/control/host"
	"github.com/Tangerg/frame/sqlite"
	"path/filepath"
	"testing"
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
	if _, err = controlhost.NewBridge(controlhost.BridgeConfig{Host: host, Store: b, Source: testModules{}}); err == nil {
		t.Fatal("substituted an unrelated store at the same epoch")
	}
}
