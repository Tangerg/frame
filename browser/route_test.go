package browser_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

// routedUse acquires a Use whose single root is a native event export.
func routedUse(t *testing.T, e *bridgeEnvironment) (browser.UseGrant, browser.LocalExport) {
	t.Helper()
	event, err := framehost.NewEvent[string]("test.ping", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.host.Register(event); err != nil {
		t.Fatal(err)
	}
	plugin, err := framehost.Define(framehost.Definition{Revision: value.Hash([]byte("test-browser-emitter")), Units: []framehost.Unit{{Declaration: framehost.UnitDeclaration{ID: "emitter", Profile: framehost.BrowserJS, Exports: []framehost.ExportDeclaration{{Name: "ping", Contract: event.Ref()}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := e.bridge.OpenRealm(e.ctx, "open-route", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	e.credentials = credentials
	if _, err := e.host.Install(e.ctx, framehost.InstallationConfig{ID: "emitter", Plugin: plugin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	root := framehost.ExportRef{InstallationID: "emitter", Release: plugin.Revision(), UnitID: "emitter", Export: "ping", Contract: event.Ref()}
	plan, err := e.host.PlanWith(e.ctx, browser.RealmID(credentials.Realm), nil, root)
	if err != nil {
		t.Fatal(err)
	}
	type acquisition struct {
		grant browser.UseGrant
		err   error
	}
	acquired := make(chan acquisition, 1)
	go func() {
		grant, err := e.bridge.AcquirePlan(e.ctx, credentials.Token, "acquire-route", plan, "")
		acquired <- acquisition{grant, err}
	}()
	local := e.pendingPlan("emitter")
	e.admit(local)
	unit := local.Units[0]
	export := browser.LocalExport{InstanceID: unit.Instance.ID, Name: "ping", Contract: event.Ref()}
	report := browser.ReadyReport{OperationID: "ready:emitter", Authority: local.Authority, Realm: local.Realm, Execution: local.Execution, Objects: []browser.ObjectRef{{Export: export, ObjectID: "emitter:ping"}}, Contributions: []browser.PreparedContribution{}}
	if result, err := e.bridge.Ready(e.ctx, credentials.Token, report); err != nil || result.Decision != browser.Accepted {
		t.Fatalf("ready: %+v %v", result, err)
	}
	result := <-acquired
	if result.err != nil {
		t.Fatal(result.err)
	}
	return result.grant, export
}

func (e *bridgeEnvironment) pendingPlan(installation framehost.InstallationID) browser.LocalPlan {
	e.t.Helper()
	for {
		projection, err := e.bridge.Poll(e.ctx, e.credentials.Token)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, plan := range projection.Plans {
			if plan.Units[0].Instance.Source.InstallationID == installation {
				return plan
			}
		}
		select {
		case <-e.ctx.Done():
			e.t.Fatal(e.ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}

func TestRouteGrantsEventDeliveryWithinItsOriginalUse(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "route.db"), true)
	grant, export := routedUse(t, e)
	route, err := e.bridge.Route(e.ctx, e.credentials.Token, grant, export, export)
	if err != nil {
		t.Fatal(err)
	}
	if route.Use.ID != grant.ID || route.Source != export || route.Receiver != export {
		t.Fatalf("route changed its original grant: %+v", route)
	}
	if err := e.bridge.ValidateRoute(e.ctx, e.credentials.Token, route); err != nil {
		t.Fatalf("issued route is not valid: %v", err)
	}
	forged := route
	forged.Receiver.Name = "forged"
	if err := e.bridge.ValidateRoute(e.ctx, e.credentials.Token, forged); !errors.Is(err, framehost.ErrStaleBinding) {
		t.Fatalf("relabelled route was accepted: %v", err)
	}
}

func TestIssuedRoutesCountAgainstPendingCapacity(t *testing.T) {
	const capacity = 4
	e := openConfiguredBridge(t, filepath.Join(t.TempDir(), "route-capacity.db"), true, func(config *browser.BridgeConfig) { config.MaxPending = capacity })
	grant, export := routedUse(t, e)
	undeclared := export
	undeclared.Name = "undeclared"
	if _, err := e.bridge.Route(e.ctx, e.credentials.Token, grant, export, undeclared); !errors.Is(err, framehost.ErrStaleBinding) {
		t.Fatalf("route outside its grant: %v", err)
	}
	for range capacity {
		if _, err := e.bridge.Route(e.ctx, e.credentials.Token, grant, export, export); err != nil {
			t.Fatalf("route within capacity: %v", err)
		}
	}
	if _, err := e.bridge.Route(e.ctx, e.credentials.Token, grant, export, export); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("route beyond capacity: %v", err)
	}
}
