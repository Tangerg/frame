package mcp

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

func TestManagedConsumerGoexitCannotAcknowledgeAChunk(t *testing.T) {
	method, err := frame.NewStreamingMethod[string, string, string](frame.MethodConfig{Name: "read", Kind: frame.Query})
	if err != nil {
		t.Fatal(err)
	}
	call, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	delivery := managedDelivery{call: call, abort: cancel, method: method, maxBytes: 1024, emit: func(context.Context, value.JSON) error {
		runtime.Goexit()
		return nil
	}}
	chunk, err := value.EncodeJSON("content")
	if err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = delivery.receive(t.Context(), 1, chunk)
	}()
	<-exited
	if err := delivery.finish(); err == nil || context.Cause(call) == nil || delivery.received != 0 {
		t.Fatalf("consumer exit acknowledged delivery: sequence=%d error=%v", delivery.received, err)
	}
}

func TestManagedGrantGoexitRetainsUnprovenPermission(t *testing.T) {
	lifetime := frame.NewLifetime(t.Context())
	lease, err := reserveManagedLease(lifetime)
	if err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_, _ = lease.issue(func() (*ManagedCallback, error) {
			runtime.Goexit()
			return nil, nil
		})
	}()
	<-exited
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	outcome, err := lifetime.Close(ctx)
	if outcome.Released || !errors.Is(err, frame.ErrUnconfirmedRelease) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("missing broker result became release evidence: %+v %v", outcome, err)
	}
}

func TestManagedRevocationGoexitStillObservesOriginalRelease(t *testing.T) {
	lifetime := frame.NewLifetime(t.Context())
	lease, err := reserveManagedLease(lifetime)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	release := sync.OnceFunc(func() { close(released) })
	defer release()
	lease.bind(&ManagedCallback{Released: released, Revoke: func() error {
		runtime.Goexit()
		return nil
	}})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := lease.finish(ctx); !errors.Is(err, frame.ErrUnconfirmedRelease) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("revocation exit did not publish its unconfirmed result: %v", err)
	}
	release()
	ctx, waitCancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer waitCancel()
	outcome, err := lifetime.BeginClose(frame.ErrClosed).WaitReleased(ctx)
	if !outcome.Released || errors.Is(err, frame.ErrUnconfirmedRelease) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("original release proof lost after revocation exit: %+v %v", outcome, err)
	}
}

func TestManagedBrokerPanicRetainsItsLocalCauseWithoutExposingIt(t *testing.T) {
	failure := errors.New("secret callback credential")
	for _, phase := range []string{"issue", "revoke"} {
		t.Run(phase, func(t *testing.T) {
			lifetime := frame.NewLifetime(t.Context())
			lease, err := reserveManagedLease(lifetime)
			if err != nil {
				t.Fatal(err)
			}
			if phase == "issue" {
				_, err = lease.issue(func() (*ManagedCallback, error) { panic(failure) })
			} else {
				released := make(chan struct{})
				close(released)
				lease.bind(&ManagedCallback{Released: released, Revoke: func() error { panic(failure) }})
				err = lease.finish(t.Context())
			}
			if err == nil || !errors.Is(err, failure) || strings.Contains(err.Error(), failure.Error()) {
				t.Fatalf("broker panic lost its private local cause: %v", err)
			}
		})
	}
}

func TestManagedConsumerPanicRetainsItsLocalCauseWithoutExposingIt(t *testing.T) {
	method, err := frame.NewStreamingMethod[string, string, string](frame.MethodConfig{Name: "read", Kind: frame.Query})
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := value.EncodeJSON("content")
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []error{errors.New("private consumer state"), &Refusal{Code: "private", Message: "private refusal details"}} {
		t.Run(failure.Error(), func(t *testing.T) {
			call, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			delivery := managedDelivery{call: call, abort: cancel, method: method, maxBytes: 1024, emit: func(context.Context, value.JSON) error {
				panic(failure)
			}}
			err := delivery.receive(t.Context(), 1, chunk)
			if err == nil || !errors.Is(err, failure) || strings.Contains(err.Error(), failure.Error()) {
				t.Fatalf("consumer panic lost its private local cause: %v", err)
			}
			if _, accepted := encodeCallbackReply(value.JSON{}, err, CallbackReceiverLimits{MaxResponseBytes: 1024}); accepted {
				t.Fatal("private panic cause escaped as a business refusal")
			}
			if err := delivery.finish(); !errors.Is(err, failure) || strings.Contains(err.Error(), failure.Error()) {
				t.Fatalf("finished delivery lost its private local cause: %v", err)
			}
		})
	}
}
