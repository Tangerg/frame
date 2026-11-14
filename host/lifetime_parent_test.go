package host_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
)

type delayedCancellation struct {
	done    chan struct{}
	deliver chan struct{}
	mu      sync.Mutex
	err     error
	after   []*func()
}

func (*delayedCancellation) Deadline() (time.Time, bool) { return time.Time{}, false }
func (p *delayedCancellation) Done() <-chan struct{}     { return p.done }
func (p *delayedCancellation) Err() error                { p.mu.Lock(); defer p.mu.Unlock(); return p.err }
func (*delayedCancellation) Value(any) any               { return nil }

func (p *delayedCancellation) AfterFunc(callback func()) func() bool {
	p.mu.Lock()
	p.after = append(p.after, &callback)
	p.mu.Unlock()
	return func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		if callback == nil {
			return false
		}
		callback = nil
		return true
	}
}

func (p *delayedCancellation) cancel() {
	p.mu.Lock()
	p.err = context.Canceled
	close(p.done)
	var callbacks []func()
	for _, pending := range p.after {
		if *pending != nil {
			callbacks = append(callbacks, *pending)
			*pending = nil
		}
	}
	p.after = nil
	p.mu.Unlock()
	for _, callback := range callbacks {
		go func() { <-p.deliver; callback() }()
	}
}

func TestParentCancellationFencesAdmissionBeforeNotification(t *testing.T) {
	parent := &delayedCancellation{done: make(chan struct{}), deliver: make(chan struct{})}
	lifetime := frame.NewLifetime(parent)
	host, err := framehost.New(context.Background(), framehost.Config{Lifetime: lifetime})
	if err != nil {
		t.Fatal(err)
	}
	work, err := lifetime.Enter(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	parent.cancel()
	if extra, err := lifetime.Enter(t.Context()); !errors.Is(err, frame.ErrClosed) {
		if extra.Valid() {
			extra.Finish()
		}
		t.Errorf("parent cancellation admitted new work: %v", err)
	}
	if _, err := lifetime.Own(frame.Resource{Name: "new resource", Close: func(context.Context) (frame.ReleaseState, error) {
		return frame.ResourceReleased, nil
	}}); !errors.Is(err, frame.ErrClosed) {
		t.Errorf("parent cancellation admitted a new resource: %v", err)
	}
	contract, err := framehost.NewContract[string]("parent.cancellation", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(contract); !errors.Is(err, frame.ErrClosed) {
		t.Errorf("parent cancellation admitted a new contract: %v", err)
	}
	var canceled, released atomic.Int64
	if _, err := work.Own(frame.Resource{
		Name:   "original work resource",
		Cancel: func(error) { canceled.Add(1) },
		Close: func(context.Context) (frame.ReleaseState, error) {
			released.Add(1)
			return frame.ResourceReleased, nil
		},
	}); err != nil {
		t.Errorf("original work lost its resource ownership: %v", err)
	}
	close(parent.deliver)
	work.Finish()
	if outcome, err := host.Close(testContext(t)); err != nil || !outcome.Released {
		t.Fatalf("parent retirement did not join original work: %+v, %v", outcome, err)
	}
	if canceled.Load() != 1 || released.Load() != 1 {
		t.Fatalf("original resource cancellation=%d, release=%d", canceled.Load(), released.Load())
	}
}

func TestOriginalParentCauseSurvivesDelayedNotification(t *testing.T) {
	parent := &delayedCancellation{done: make(chan struct{}), deliver: make(chan struct{})}
	owner := frame.NewLifetime(parent)
	work, err := owner.Enter(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		close(parent.deliver)
		work.Finish()
		if outcome, err := owner.Close(testContext(t)); !outcome.Released || err != nil {
			t.Errorf("original close: %+v, %v", outcome, err)
		}
	}()
	original := owner.Context()
	parent.cancel()
	owner.BeginClose(frame.ErrClosed)
	if !errors.Is(context.Cause(original), context.Canceled) || !errors.Is(context.Cause(work.Context()), context.Canceled) {
		t.Fatalf("a queued notification replaced the parent cause: context=%v work=%v", context.Cause(original), context.Cause(work.Context()))
	}
}
