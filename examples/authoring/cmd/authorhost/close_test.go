package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

func TestSealRouteClosesOriginalRealmAfterActivationWaitWasCancelled(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), closeTimeout)
	defer cancel()
	admission := frame.NewAdmission()
	store, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(t.TempDir(), "author.db"), Admission: admission})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	host, err := frame.NewHost(ctx, frame.HostConfig{Admission: admission, Epoch: store.Epoch(), Installations: store, ExecutionProtection: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := closeHost(host); err != nil {
			t.Error(err)
		}
	})
	module := []byte("author fixture module")
	plugin, err := frame.Define(frame.Definition{Revision: value.Hash(module), Units: []frame.Unit{{Declaration: frame.UnitDeclaration{ID: "view", Profile: frame.BrowserJS}}}})
	if err != nil {
		t.Fatal(err)
	}
	bridge, err := browser.NewBridge(browser.BridgeConfig{Host: host, Store: store, Source: moduleSource{release: plugin.Revision(), digest: value.Hash(module)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := host.RegisterDriver(bridge); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Install(ctx, frame.InstallationConfig{ID: installationID, Plugin: plugin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	credentials, err := bridge.OpenRealm(ctx, "author-client", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := bridge.SealRealm(context.Background(), credentials.Token); err != nil {
			t.Error(err)
		}
	})
	other, err := bridge.OpenRealm(ctx, "other-client", "fedcba9876543210fedcba9876543210")
	if err != nil {
		t.Fatal(err)
	}
	handler := routes(host, bridge, author{plugin: plugin, module: module}, credentials)
	for _, token := range []string{"", other.Token} {
		request := httptest.NewRequestWithContext(ctx, http.MethodPost, string(sealRealmPath), strings.NewReader("{}"))
		request.Header.Set("Authorization", "Bearer "+token)
		reply := httptest.NewRecorder()
		handler.ServeHTTP(reply, request)
		if reply.Code != http.StatusForbidden {
			t.Fatalf("foreign application credential sealed Realm: status %d", reply.Code)
		}
	}
	activationContext, cancelActivation := context.WithCancel(ctx)
	defer cancelActivation()
	request := httptest.NewRequestWithContext(activationContext, http.MethodPost, string(activatePath), strings.NewReader(`{"unit":"view"}`))
	request.Header.Set("Authorization", "Bearer "+credentials.Token)
	activation := httptest.NewRecorder()
	activated := make(chan struct{})
	go func() {
		handler.ServeHTTP(activation, request)
		close(activated)
	}()
	var plan browser.LocalPlan
	for plan.Execution.ID == "" {
		pending, err := bridge.Poll(ctx, credentials.Token)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending.Plans) == 1 {
			plan = pending.Plans[0]
			break
		}
		select {
		case <-activated:
			t.Fatalf("activation finished before an approved plan: %d %s", activation.Code, activation.Body)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	cancelActivation()
	select {
	case <-activated:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if activation.Code != http.StatusConflict {
		t.Fatalf("cancelled activation waiter returned %d", activation.Code)
	}
	pending, err := bridge.Poll(ctx, credentials.Token)
	if err != nil || len(pending.Plans) != 1 || pending.Plans[0].Execution != plan.Execution {
		t.Fatalf("wait cancellation was mistaken for execution exit: %+v %v", pending, err)
	}
	invalidSeal := httptest.NewRequestWithContext(ctx, http.MethodPost, string(sealRealmPath), strings.NewReader(`{"token":"`+other.Token+`"}`))
	invalidSeal.Header.Set("Authorization", "Bearer "+credentials.Token)
	refused := httptest.NewRecorder()
	handler.ServeHTTP(refused, invalidSeal)
	if refused.Code != http.StatusBadRequest {
		t.Fatalf("seal accepted undeclared input: %d %s", refused.Code, refused.Body)
	}
	for range 2 {
		request := httptest.NewRequestWithContext(ctx, http.MethodPost, string(sealRealmPath), strings.NewReader(`{}`))
		request.Header.Set("Authorization", "Bearer "+credentials.Token)
		reply := httptest.NewRecorder()
		handler.ServeHTTP(reply, request)
		if reply.Code != http.StatusNoContent || reply.Body.Len() != 0 {
			t.Fatalf("seal did not acknowledge original Realm: %d %s", reply.Code, reply.Body)
		}
	}
	if err := host.WaitInstanceRetired(ctx, plan.Execution.ID); err != nil {
		t.Fatal(err)
	}
	record, err := store.Attempt(ctx, plan.Execution.ID)
	if err != nil || !record.ExitProven() || record.Proof.Kind != "not-admitted" {
		t.Fatalf("unobserved plan was not retired by its Host owner: %+v %v", record, err)
	}
	dependencies, err := store.ExecutionDependencies(ctx)
	if err != nil || len(dependencies) != 0 {
		t.Fatalf("sealed plan retained dependencies: %+v %v", dependencies, err)
	}
	_, err = host.ActivateUnit(ctx, browser.RealmID(credentials.Realm), frame.UnitRef{InstallationID: installationID, Release: plugin.Revision(), UnitID: "view"})
	if !errors.Is(err, frame.ErrFenced) {
		t.Fatalf("sealed Realm admitted another activation: %v", err)
	}
	reconnected, err := bridge.Reconnect(ctx, other.Token, browser.ReconnectRequest{Authority: other.Authority, Realm: other.Realm, Executions: []browser.Execution{}})
	if err != nil || reconnected.Mode != browser.Revalidated {
		t.Fatalf("seal changed the Realm named by untrusted request data: %+v %v", reconnected, err)
	}
}
