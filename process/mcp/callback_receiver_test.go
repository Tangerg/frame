package mcp

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

func callbackTestMethod(t *testing.T) *frame.Method {
	t.Helper()
	method, err := frame.AdmitMethod(frame.MethodDescriptor{Name: "document.read", Kind: frame.Command, Input: []byte(`{"type":"string"}`), Output: []byte(`{"type":"string"}`)})
	if err != nil {
		t.Fatal(err)
	}
	return method
}

func callbackBody(t *testing.T, method *frame.Method, input string) []byte {
	t.Helper()
	b, err := json.Marshal(managedParams{Protocol: managedVersion, Method: method.Name(), Contract: method.Identity().Digest(), CommandID: "original", Input: []byte(input)})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func callbackPost(ctx context.Context, ref ManagedCallback, body []byte) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ref.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", ref.Credential)
	req.Header.Set("Qimu-Call", ref.Call)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), err
}

func TestReceiverEnforcesGrantBeforeInvokingHost(t *testing.T) {
	limits := CallbackReceiverLimits{1, 1024, 32, time.Second}
	r, err := NewCallbackReceiver(limits)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	method := callbackTestMethod(t)
	handler := func(_ context.Context, id string, input value.JSON) (value.JSON, error) {
		if id != "original" {
			t.Error("changed command identity")
		}
		calls.Add(1)
		return input, nil
	}
	ref, err := r.Grant(t.Context(), method, handler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ref.Revoke(); err != nil {
			t.Error(err)
		}
	})
	if _, err = r.Grant(t.Context(), method, handler); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("capacity: %v", err)
	}
	body := callbackBody(t, method, `"ok"`)
	wrongCred, wrongCall := ref, ref
	wrongCred.Credential = strings.Repeat("0", 64)
	wrongCall.Call = strings.Repeat("0", 64)
	for _, tc := range []struct {
		name   string
		ref    ManagedCallback
		body   []byte
		status int
	}{
		{"credential", wrongCred, body, 403}, {"call", wrongCall, body, 403},
		{"unknown-field", ref, bytes.Replace(body, []byte(`"protocol"`), []byte(`"extra":1,"protocol"`), 1), 400},
		{"wrong-identity", ref, bytes.Replace(body, []byte(method.Identity().Digest().String()), []byte(strings.Repeat("0", 64)), 1), 400},
		{"missing-command", ref, bytes.Replace(body, []byte(`"original"`), []byte(`""`), 1), 400},
		{"wrong-input", ref, callbackBody(t, method, `true`), 422},
		{"request-bytes", ref, bytes.Repeat([]byte{' '}, 1025), 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, _, err := callbackPost(t.Context(), tc.ref, tc.body)
			if err != nil || status != tc.status {
				t.Fatalf("status=%d err=%v", status, err)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("unauthorized callback invoked")
	}
	status, reply, err := callbackPost(t.Context(), ref, body)
	if err != nil || status != 200 || reply != `{"value":"ok"}` {
		t.Fatalf("valid callback: %d %s %v", status, reply, err)
	}
	status, _, err = callbackPost(t.Context(), ref, callbackBody(t, method, `"`+strings.Repeat("x", 30)+`"`))
	if err != nil || status != 422 {
		t.Fatalf("response envelope budget: %d %v", status, err)
	}
}

func TestReceiverReleaseJoinsCanceledCallAndRejectsStaleTokens(t *testing.T) {
	r, err := NewCallbackReceiver(CallbackReceiverLimits{1, 1024, 1024, time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	method := callbackTestMethod(t)
	body := callbackBody(t, method, `"ok"`)
	entered, canceled, drain := make(chan struct{}), make(chan struct{}), make(chan struct{})
	ref, err := r.Grant(ctx, method, func(call context.Context, _ string, input value.JSON) (value.JSON, error) {
		close(entered)
		<-call.Done()
		close(canceled)
		<-drain
		return input, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	requestDone := make(chan struct{})
	go func() { defer close(requestDone); _, _, _ = callbackPost(t.Context(), ref, body) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("callback did not enter")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("owner did not cancel active callback")
	}
	select {
	case <-ref.Released:
		t.Fatal("acknowledged release while original host callback was live")
	default:
	}
	close(drain)
	if err = ref.Revoke(); err != nil {
		t.Fatal(err)
	}
	<-requestDone
	next, err := r.Grant(t.Context(), method, func(_ context.Context, _ string, in value.JSON) (value.JSON, error) { return in, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer next.Revoke()
	if next.Credential == ref.Credential || next.Call == ref.Call {
		t.Fatal("reissued callback credentials")
	}
	stale := ref
	stale.Endpoint = next.Endpoint
	if status, _, err := callbackPost(t.Context(), stale, body); err != nil || status != 403 {
		t.Fatalf("previous call token: status=%d err=%v", status, err)
	}
}

func TestReceiverLimitsAreExplicit(t *testing.T) {
	for _, l := range []CallbackReceiverLimits{{}, {0, 1, 1, time.Second}, {1, 0, 1, time.Second}, {1, 1, 0, time.Second}, {1, 1, 1, 0}} {
		if _, err := NewCallbackReceiver(l); !errors.Is(err, frame.ErrInvalid) {
			t.Fatal(l, err)
		}
	}
}
