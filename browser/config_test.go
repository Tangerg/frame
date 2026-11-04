package browser_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
)

type nilRemoteSource struct{}

func (*nilRemoteSource) Import(context.Context, *frame.Setup, frame.BoundValue) (browser.RemoteImport, error) {
	return browser.RemoteImport{}, nil
}

func TestBridgeRejectsTypedNilAdapters(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true)
	for _, test := range []struct {
		name   string
		source browser.ModuleSource
		remote browser.RemoteSource
	}{
		{name: "module", source: (*testModules)(nil)},
		{name: "remote", source: testModules{}, remote: (*nilRemoteSource)(nil)},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := browser.NewBridge(browser.BridgeConfig{Host: e.host, Store: e.store, Source: test.source, Remote: test.remote})
			if !errors.Is(err, frame.ErrInvalid) {
				t.Fatalf("typed nil adapter was admitted: %v", err)
			}
		})
	}
}
