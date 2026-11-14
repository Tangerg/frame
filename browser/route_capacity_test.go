package browser_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

func TestRefusedRouteDoesNotConsumeHostCapacity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	admission := framehost.NewAdmission()
	store, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(t.TempDir(), "routes.db"), Admission: admission})
	if err != nil {
		t.Fatal(err)
	}
	host, err := framehost.New(ctx, framehost.Config{Authority: store, Limits: framehost.Limits{MaxEventRoutes: 1}})
	if err != nil {
		t.Fatal(err)
	}
	bridge, err := browser.NewBridge(browser.BridgeConfig{Host: host, Store: store, Source: testModules{}, MaxMessageBytes: 6500})
	if err != nil {
		t.Fatal(err)
	}
	if err := host.RegisterDriver(bridge); err != nil {
		t.Fatal(err)
	}
	credentials, err := bridge.OpenRealm(ctx, "route-realm", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	e := &bridgeEnvironment{t: t, ctx: ctx, store: store, host: host, bridge: bridge, credentials: credentials}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		for _, plan := range e.plans {
			if _, err := bridge.Prove(cleanup, credentials.Token, executionProof(plan)); err != nil {
				t.Error(err)
			}
			if err := host.WaitInstanceRetired(cleanup, plan.Execution.ID); err != nil {
				t.Error(err)
			}
		}
		if result, err := host.Close(cleanup); !result.Released {
			t.Errorf("Host cleanup: %+v, %v", result, err)
		}
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	acquire := func(id, name, version string) (browser.UseGrant, browser.LocalExport) {
		t.Helper()
		event, err := framehost.NewEvent[string](name, version)
		if err != nil {
			t.Fatal(err)
		}
		if err := host.Register(event); err != nil {
			t.Fatal(err)
		}
		plugin, err := framehost.Define(framehost.Definition{Revision: value.Hash([]byte(id)), Units: []framehost.Unit{{Declaration: framehost.UnitDeclaration{ID: "emitter", Profile: framehost.BrowserJS, Exports: []framehost.ExportDeclaration{{Name: "ping", Contract: event.Ref()}}}}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := host.Install(ctx, framehost.InstallationConfig{ID: framehost.InstallationID(id), Plugin: plugin, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		root := framehost.ExportRef{InstallationID: framehost.InstallationID(id), Release: plugin.Revision(), UnitID: "emitter", Export: "ping", Contract: event.Ref()}
		plan, err := host.PlanWith(ctx, browser.RealmID(credentials.Realm), nil, root)
		if err != nil {
			t.Fatal(err)
		}
		type result struct {
			grant browser.UseGrant
			err   error
		}
		acquired := make(chan result, 1)
		go func() {
			grant, err := bridge.AcquirePlan(ctx, credentials.Token, "acquire-"+id, plan, "")
			acquired <- result{grant, err}
		}()
		local := e.pendingPlan(framehost.InstallationID(id))
		e.admit(local)
		export := browser.LocalExport{InstanceID: local.Units[0].Instance.ID, Name: "ping", Contract: event.Ref()}
		report := browser.ReadyReport{OperationID: "ready-" + id, Authority: local.Authority, Realm: local.Realm, Execution: local.Execution, Objects: []browser.ObjectRef{{Export: export, ObjectID: id + "-object"}}, Contributions: []browser.PreparedContribution{}}
		if result, err := bridge.Ready(ctx, credentials.Token, report); err != nil || result.Decision != browser.Accepted {
			t.Fatalf("ready %s: %+v %v", id, result, err)
		}
		resultValue := <-acquired
		if resultValue.err != nil {
			t.Fatalf("acquire %s: %v", id, resultValue.err)
		}
		return resultValue.grant, export
	}
	large, largeExport := acquire("large", strings.Repeat("<", frame.MaxNameBytes), strings.Repeat("<", frame.MaxNameBytes))
	small, smallExport := acquire("small", "test.short-event", "1")
	if _, err := bridge.Route(ctx, credentials.Token, large, largeExport, largeExport); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("large route expected bounded refusal: %v", err)
	}
	route, err := bridge.Route(ctx, credentials.Token, small, smallExport, smallExport)
	if err != nil {
		t.Fatalf("refused route retained the original Host slot: %v", err)
	}
	if err := bridge.ValidateRoute(ctx, credentials.Token, route); err != nil {
		t.Fatalf("valid route did not retain its original grant: %v", err)
	}
	for index, grant := range []browser.UseGrant{large, small} {
		if _, err := bridge.Prove(ctx, credentials.Token, useProof(e.plans[index], grant)); err != nil {
			t.Fatal(err)
		}
	}
}
