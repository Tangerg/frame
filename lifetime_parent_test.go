package frame_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/frame"
)

type delayedCancellation struct {
	done      chan struct{}
	deliver   chan struct{}
	mu        sync.Mutex
	err       error
	after     func()
	scheduled bool
}

func (*delayedCancellation) Deadline() (time.Time, bool) { return time.Time{}, false }
func (p *delayedCancellation) Done() <-chan struct{}     { return p.done }
func (p *delayedCancellation) Err() error                { p.mu.Lock(); defer p.mu.Unlock(); return p.err }
func (*delayedCancellation) Value(any) any               { return nil }

func (p *delayedCancellation) AfterFunc(callback func()) func() bool {
	p.mu.Lock()
	p.after = callback
	p.mu.Unlock()
	return func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.scheduled || p.after == nil {
			return false
		}
		p.after = nil
		return true
	}
}

func (p *delayedCancellation) cancel() {
	p.mu.Lock()
	p.err = context.Canceled
	close(p.done)
	callback := p.after
	p.scheduled = callback != nil
	p.mu.Unlock()
	if callback != nil {
		go func() { <-p.deliver; callback() }()
	}
}

func TestParentCancellationFencesAdmissionBeforeNotification(t *testing.T) {
	parent := &delayedCancellation{done: make(chan struct{}), deliver: make(chan struct{})}
	lifetime := frame.NewLifetime(parent)
	host, err := frame.NewHost(context.Background(), frame.HostConfig{Lifetime: lifetime})
	if err != nil {
		t.Fatal(err)
	}
	work, err := lifetime.Enter(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	parent.cancel()
	if extra, err := lifetime.Enter(t.Context()); !errors.Is(err, frame.ErrClosed) {
		if extra != nil {
			extra.Finish()
		}
		t.Errorf("parent cancellation admitted new work: %v", err)
	}
	if _, err := lifetime.Own(frame.Resource{Name: "new resource", Close: func(context.Context) (frame.ReleaseState, error) {
		return frame.ResourceReleased, nil
	}}); !errors.Is(err, frame.ErrClosed) {
		t.Errorf("parent cancellation admitted a new resource: %v", err)
	}
	contract, err := frame.NewContract[string]("parent.cancellation", "1")
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
