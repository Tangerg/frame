package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Tangerg/frame/browser"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

func TestActivateRejectsInvalidCompleteBodyBeforeCreatingExecution(t *testing.T) {
	const prefix = "{\"unit\":\"view\"}"
	cases := []struct {
		name   string
		body   string
		status int
	}{
		{"second-json-value", prefix + "{\"unit\":\"ignored\"}", http.StatusBadRequest},
		{"invalid-trailing-data", prefix + " not-json", http.StatusBadRequest},
		{"body-beyond-capacity", prefix + strings.Repeat(" ", maxApplicationRequestBytes), http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), closeTimeout)
			defer cancel()
			admission := framehost.NewAdmission()
			store, err := sqlite.Open(ctx, sqlite.Config{
				Path:      filepath.Join(t.TempDir(), "author.db"),
				Admission: admission,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := store.Close(); err != nil {
					t.Error(err)
				}
			})
			host, err := framehost.New(ctx, framehost.Config{
				Authority: store,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := closeHost(host); err != nil {
					t.Error(err)
				}
			})
			module := []byte("author fixture module")
			plugin, err := framehost.Define(framehost.Definition{
				Revision: value.Hash(module),
				Units: []framehost.Unit{{
					Declaration: framehost.UnitDeclaration{ID: "view", Profile: framehost.BrowserJS},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			bridge, err := browser.NewBridge(browser.BridgeConfig{
				Host: host, Store: store,
				Source: moduleSource{release: plugin.Revision(), digest: value.Hash(module)},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := host.RegisterDriver(bridge); err != nil {
				t.Fatal(err)
			}
			if _, err := host.Install(ctx, framehost.InstallationConfig{
				ID: installationID, Plugin: plugin, Enabled: true,
			}); err != nil {
				t.Fatal(err)
			}
			credentials, err := bridge.OpenRealm(ctx, "audit-client", "0123456789abcdef0123456789abcdef")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := bridge.SealRealm(context.Background(), credentials.Token); err != nil {
					t.Error(err)
				}
			})
			handler := routes(host, bridge, author{plugin: plugin, module: module}, credentials)
			requestContext, cancelRequest := context.WithCancel(ctx)
			defer cancelRequest()
			request := httptest.NewRequestWithContext(
				requestContext, http.MethodPost, string(activatePath), strings.NewReader(tc.body),
			)
			request.Header.Set("Authorization", "Bearer "+credentials.Token)
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				handler.ServeHTTP(response, request)
			}()

			var plan browser.LocalPlan
		observe:
			for {
				projection, err := bridge.Poll(ctx, credentials.Token)
				if err != nil {
					t.Fatal(err)
				}
				if len(projection.Plans) != 0 {
					plan = projection.Plans[0]
					break
				}
				select {
				case <-done:
					break observe
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(time.Millisecond):
				}
			}

			cancelRequest()
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if err := bridge.SealRealm(ctx, credentials.Token); err != nil {
				t.Fatal(err)
			}
			if plan.Execution.ID != "" {
				if err := host.WaitInstanceRetired(ctx, plan.Execution.ID); err != nil {
					t.Fatal(err)
				}
				t.Fatalf("invalid complete request activated BrowserJS execution: bodyBytes=%d limit=%d execution=%s unit=%s responseAfterWaitCancellation=%d; expected boundary rejection before activation",
					len(tc.body), maxApplicationRequestBytes, plan.Execution.ID,
					plan.Units[0].Instance.Source.UnitID, response.Code)
			}
			if response.Code != tc.status {
				t.Fatalf("invalid body response = %d, want %d: %s", response.Code, tc.status, response.Body)
			}
		})
	}
}
