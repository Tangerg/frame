package browser_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/sqlite"
)

func beginBrowserActivation(e *bridgeEnvironment, id string) <-chan error {
	e.t.Helper()
	if e.credentials.Token == "" {
		credentials, err := e.bridge.OpenRealm(e.ctx, "capacity-realm", "0123456789abcdef0123456789abcdef")
		if err != nil {
			e.t.Fatal(err)
		}
		e.credentials = credentials
	}
	if _, err := e.host.Install(e.ctx, framehost.InstallationConfig{ID: framehost.InstallationID(id), Plugin: e.plugin, Enabled: true}); err != nil {
		e.t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.host.ActivateUnit(e.ctx, browser.RealmID(e.credentials.Realm), framehost.UnitRef{InstallationID: framehost.InstallationID(id), Release: e.plugin.Revision(), UnitID: "view"})
		done <- err
	}()
	return done
}

func controlReply(t *testing.T, bridge *browser.Bridge, token, method string, input any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(browser.HTTPEnvelope{Method: method, Input: encoded})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/control", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	bridge.Handler().ServeHTTP(response, request)
	return response
}

func TestPlanCapacityIsRefusedBeforeExposure(t *testing.T) {
	const messageBytes = 4096
	e := openConfiguredBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true, func(config *browser.BridgeConfig) {
		config.MaxMessageBytes = messageBytes
		config.PublicConfiguration = func(context.Context, *framehost.Setup) ([]byte, error) {
			return json.Marshal(strings.Repeat("x", 2*messageBytes))
		}
	})
	done := beginBrowserActivation(e, "oversized")
	if err := <-done; !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("unsendable plan was not refused: %v", err)
	}
	poll, err := e.bridge.Poll(e.ctx, e.credentials.Token)
	if err != nil || len(poll.Plans) != 0 || len(poll.Fences) != 0 {
		t.Fatalf("refused plan escaped its nondelivery owner: %+v %v", poll, err)
	}
	dependencies, err := e.store.ExecutionDependencies(e.ctx)
	if err != nil || len(dependencies) != 0 {
		t.Fatalf("unsent plan retained execution protection: %+v %v", dependencies, err)
	}
}

func TestPlanAdmissionIncludesDurableExitCapacity(t *testing.T) {
	for _, requestBytes := range []int{2048, 4096} {
		t.Run(fmt.Sprint(requestBytes), func(t *testing.T) {
			e := openBridgeEnvironment(t, sqlite.Config{Path: filepath.Join(t.TempDir(), "state.sqlite"), MaxRequestBytes: requestBytes}, true, nil)
			if err := <-beginBrowserActivation(e, "component"); !errors.Is(err, frame.ErrCapacity) {
				t.Fatalf("durably untransportable proof was admitted: %v", err)
			}
			poll, err := e.bridge.Poll(e.ctx, e.credentials.Token)
			if err != nil || len(poll.Plans) != 0 || len(poll.Fences) != 0 {
				t.Fatalf("refused proof budget exposed work: %+v %v", poll, err)
			}
			attempts, err := e.store.Attempts(e.ctx)
			if err != nil || len(attempts) != 0 {
				t.Fatalf("refused proof budget accepted durable responsibility: %+v %v", attempts, err)
			}
		})
	}
}

func TestAdmittedExecutionCanPersistMaximumExitIdentity(t *testing.T) {
	const requestBytes = 8192
	for _, spelling := range []struct{ name, character string }{
		{"HTML", "<"}, {"quote", `"`}, {"backslash", `\`},
	} {
		t.Run(spelling.name, func(t *testing.T) {
			e := openBridgeEnvironment(t, sqlite.Config{Path: filepath.Join(t.TempDir(), "state.sqlite"), MaxRequestBytes: requestBytes}, true, nil)
			plan := e.start("component")
			proof := executionProof(plan)
			proof.CloseID = strings.Repeat(spelling.character, frame.MaxNameBytes)
			proof.ProofID = proof.CloseID
			ack, err := e.bridge.Prove(e.ctx, e.credentials.Token, proof)
			if err != nil || !ack.Recorded {
				t.Fatalf("admitted execution cannot settle original proof: %+v %v", ack, err)
			}
			ack, err = e.bridge.Prove(e.ctx, e.credentials.Token, proof)
			if err != nil || !ack.Recorded {
				t.Fatalf("original proof cannot be observed: %+v %v", ack, err)
			}
			if err := e.host.WaitInstanceRetired(e.ctx, plan.Execution.ID); err != nil {
				t.Fatal(err)
			}
			e.plans = nil
		})
	}
}

func TestCompletePendingProjectionReservesEncodedBytesAndReturnsCapacityOnRelease(t *testing.T) {
	const messageBytes = 16 << 10
	e := openConfiguredBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true, func(config *browser.BridgeConfig) {
		config.MaxMessageBytes = messageBytes
		config.PublicConfiguration = func(context.Context, *framehost.Setup) ([]byte, error) {
			return json.Marshal(strings.Repeat("<", 600))
		}
	})
	var last browser.Poll
	limit := 1
	refused := false
	for index := 0; index <= limit; index++ {
		id := fmt.Sprintf("pending-%04d", index)
		done := beginBrowserActivation(e, id)
		observed := false
		for !observed && !refused {
			var err error
			last, err = e.bridge.Poll(e.ctx, e.credentials.Token)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(last)
			if err != nil || len(encoded) > messageBytes {
				t.Fatalf("complete poll cannot be transported: %d %v", len(encoded), err)
			}
			for _, plan := range last.Plans {
				if plan.Units[0].Instance.Source.InstallationID != framehost.InstallationID(id) {
					continue
				}
				observed = true
				e.plans = append(e.plans, plan)
				if index == 0 {
					encoded, err := json.Marshal(plan)
					if err != nil {
						t.Fatal(err)
					}
					limit = messageBytes/len(encoded) + 1
				}
				if response := controlReply(t, e.bridge, e.credentials.Token, "admitPlan", plan); response.Code != http.StatusOK {
					t.Fatalf("pending plan cannot be admitted: %d %s", response.Code, response.Body.String())
				}
			}
			if observed {
				break
			}
			select {
			case err := <-done:
				if !errors.Is(err, frame.ErrCapacity) {
					t.Fatalf("activation ended before exposure: %v", err)
				}
				refused = true
			case <-e.ctx.Done():
				t.Fatal(e.ctx.Err())
			case <-time.After(time.Millisecond):
			}
		}
		if refused {
			break
		}
	}
	if !refused || len(last.Plans) < 2 {
		t.Fatalf("pending bytes were not bounded as an aggregate: plans=%d refused=%t", len(last.Plans), refused)
	}
	executions := make([]browser.Execution, 0, len(last.Plans))
	for _, plan := range last.Plans {
		executions = append(executions, plan.Execution)
	}
	reconnect := browser.ReconnectRequest{Authority: e.credentials.Authority, Realm: e.credentials.Realm, Executions: executions}
	if response := controlReply(t, e.bridge, e.credentials.Token, "reconnect", reconnect); response.Code != http.StatusOK || response.Body.Len() > messageBytes {
		t.Fatalf("complete recovery cannot be transported: %d %s", response.Code, response.Body.String())
	}
	if _, err := e.bridge.Prove(e.ctx, e.credentials.Token, executionProof(last.Plans[0])); err != nil {
		t.Fatal(err)
	}
	done := beginBrowserActivation(e, "replacement")
	for {
		poll, err := e.bridge.Poll(e.ctx, e.credentials.Token)
		if err != nil {
			t.Fatal(err)
		}
		for _, plan := range poll.Plans {
			if plan.Units[0].Instance.Source.InstallationID == "replacement" {
				e.admit(plan)
				return
			}
		}
		select {
		case err := <-done:
			t.Fatalf("released projection did not return capacity: %v", err)
		case <-e.ctx.Done():
			t.Fatal(e.ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}

func TestHTTPMessageLimitIncludesTheCompleteEnvelope(t *testing.T) {
	const messageBytes = 8192
	e := openConfiguredBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true, func(config *browser.BridgeConfig) { config.MaxMessageBytes = messageBytes })
	e.start("host")
	if e.credentials.MaxMessageBytes != messageBytes {
		t.Fatalf("bootstrap did not project the protocol budget: %+v", e.credentials)
	}
	for _, extra := range []int{0, 1} {
		body := []byte(`{"method":"poll","input":{}}`)
		body = append(body, bytes.Repeat([]byte(" "), messageBytes+extra-len(body))...)
		request := httptest.NewRequest(http.MethodPost, "/control", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+e.credentials.Token)
		response := httptest.NewRecorder()
		e.bridge.Handler().ServeHTTP(response, request)
		want := http.StatusOK
		if extra != 0 {
			want = http.StatusRequestEntityTooLarge
		}
		if response.Code != want {
			t.Fatalf("envelope size %d returned %d, want %d: %s", len(body), response.Code, want, response.Body.String())
		}
	}
}
