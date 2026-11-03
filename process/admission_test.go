package process

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/value"
)

type admissionObservedContext struct {
	context.Context
	observe func()
}

func (c admissionObservedContext) Err() error {
	err := c.Context.Err()
	c.observe()
	return err
}

func TestCancellationWhileWaitingForAdmissionDoesNotDispatch(t *testing.T) {
	method, err := portable.NewMethod[string, string](portable.MethodConfig{Name: "write", Kind: portable.Command})
	if err != nil {
		t.Fatal(err)
	}
	contract, err := portable.NewCapability("test.writer", method.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	input, err := value.EncodeJSON("effect")
	if err != nil {
		t.Fatal(err)
	}
	base, cancel := context.WithCancel(t.Context())
	defer cancel()
	observed := make(chan struct{})
	ctx := admissionObservedContext{base, sync.OnceFunc(func() { close(observed) })}
	p := newPeer(nil, profile.MaxFrameBytes, nil)
	service := &remote{peer: p, handle: "writer", contract: contract}
	p.mu.Lock()
	finished := make(chan error, 1)
	go func() {
		_, err := service.Invoke(ctx, commandInvocation(t, "original-effect", "write", input))
		finished <- err
	}()
	<-observed
	cancel()
	p.mu.Unlock()
	if err := <-finished; !errors.Is(err, context.Canceled) || errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("pre-admission cancellation: %v", err)
	}
	if len(p.out) != 0 || len(p.pending) != 0 || p.next != 0 {
		t.Fatalf("cancelled call consumed admission state: frames=%d pending=%d next=%d", len(p.out), len(p.pending), p.next)
	}
}

type pausedPayload struct {
	started         chan struct{}
	continueMarshal <-chan struct{}
}

func (p pausedPayload) MarshalJSON() ([]byte, error) {
	close(p.started)
	<-p.continueMarshal
	return []byte(`{}`), nil
}

func TestCancellationDuringPayloadEncodingDoesNotDispatch(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	// The sender accepts any JSON payload; a blocked encoder exercises the gap
	// before admission without depending on scheduler timing or payload size.
	continued := make(chan struct{})
	payload := pausedPayload{started: make(chan struct{}), continueMarshal: continued}
	p := newPeer(nil, profile.MaxFrameBytes, nil)
	finished := make(chan error, 1)
	go func() {
		var result bool
		finished <- p.request(ctx, profile.Methods.Call, payload, &result)
	}()
	<-payload.started
	cause := errors.New("caller withdrew the request")
	cancel(cause)
	close(continued)
	if err := <-finished; !errors.Is(err, cause) || errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("encoding cancellation: %v", err)
	}
	if len(p.out) != 0 || len(p.pending) != 0 || p.next != 0 {
		t.Fatalf("cancelled encoding consumed admission state: frames=%d pending=%d next=%d", len(p.out), len(p.pending), p.next)
	}
}
