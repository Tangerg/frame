package browser_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
	framehost "github.com/Tangerg/frame/host"
)

type nilRemoteSource struct{}

func (*nilRemoteSource) Import(context.Context, framehost.Setup, framehost.BoundValue) (browser.RemoteImport, error) {
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

func TestBridgeRejectsUnusablePointConfiguration(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true)
	service, err := framehost.NewContract[string]("test.service", "1")
	if err != nil {
		t.Fatal(err)
	}
	otherVersion, err := framehost.NewPoint[testMetadata, browser.Object](e.first.Ref().Name, "2")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		points []framehost.ContractRef
	}{
		{name: "invalid reference", points: []framehost.ContractRef{{}}},
		{name: "service contract", points: []framehost.ContractRef{service.Ref()}},
		{name: "duplicate point", points: []framehost.ContractRef{e.first.Ref(), e.first.Ref()}},
		{name: "ambiguous versions", points: []framehost.ContractRef{e.first.Ref(), otherVersion.Ref()}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := browser.NewBridge(browser.BridgeConfig{Host: e.host, Store: e.store, Source: testModules{}, Points: test.points})
			if !errors.Is(err, frame.ErrInvalid) {
				t.Fatalf("unusable point configuration was admitted: %v", err)
			}
		})
	}
	for _, points := range [][]framehost.ContractRef{nil, {e.first.Ref(), e.second.Ref()}} {
		if _, err := browser.NewBridge(browser.BridgeConfig{Host: e.host, Store: e.store, Source: testModules{}, Points: points}); err != nil {
			t.Fatalf("valid point configuration was refused: %v", err)
		}
	}
}
