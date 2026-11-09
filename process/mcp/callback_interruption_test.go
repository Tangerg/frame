package mcp

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Tangerg/frame/value"
)

func TestReceiverRetainsRedactedHandlerInterruption(t *testing.T) {
	cause := errors.New("private callback diagnostic")
	refusal := &Refusal{Code: "private-code", Message: "private refusal diagnostic"}
	for _, test := range []struct {
		name      string
		interrupt func()
		cause     error
	}{
		{"panic-error", func() { panic(cause) }, cause},
		{"panic-refusal", func() { panic(refusal) }, refusal},
		{"panic-text", func() { panic("private callback text") }, nil},
		{"goexit", runtime.Goexit, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			var diagnostics bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&diagnostics)
			defer log.SetOutput(previous)
			receiver, err := NewCallbackReceiver(CallbackReceiverLimits{1, 4096, 4096, time.Second})
			if err != nil {
				t.Fatal(err)
			}
			method := callbackTestMethod(t)
			grant, err := receiver.Grant(t.Context(), method, func(context.Context, string, value.JSON) (value.JSON, error) {
				test.interrupt()
				return value.JSON{}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			defer grant.Revoke()
			status, body, callErr := callbackPost(t.Context(), grant, callbackBody(t, method, `"input"`))
			if callErr == nil && status != http.StatusUnprocessableEntity {
				t.Fatalf("interrupted callback reported success: %d %s", status, body)
			}
			failure := grant.Revoke()
			if failure == nil || test.cause != nil && !errors.Is(failure, test.cause) {
				t.Fatalf("original grant lost interruption: %v", failure)
			}
			if strings.Contains(failure.Error(), "private") || strings.Contains(body, "private") || strings.Contains(diagnostics.String(), "private") {
				t.Fatalf("interruption exposed private diagnostic: error=%v body=%q log=%q", failure, body, diagnostics.String())
			}
			select {
			case <-grant.Released:
			default:
				t.Fatal("joined callback did not release its original grant")
			}
		})
	}
}
