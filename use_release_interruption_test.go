package frame_test

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/Tangerg/frame"
)

type interruptedUseRelease struct {
	release func() error
	calls   atomic.Int64
}

func (*interruptedUseRelease) ApproveUse(context.Context, frame.UseApproval) error { return nil }

func (p *interruptedUseRelease) ReleaseUse(context.Context, string, frame.ReleaseProof) error {
	if p.calls.Add(1) == 1 {
		return p.release()
	}
	return nil
}

func TestUseReleaseGoexitPreservesUnsettledProtection(t *testing.T) {
	protection := &interruptedUseRelease{release: func() error { runtime.Goexit(); return nil }}
	testInterruptedUseRelease(t, protection, nil)
}

func TestUseReleasePanicPreservesOriginalCause(t *testing.T) {
	cause := errors.New("original release failure")
	protection := &interruptedUseRelease{release: func() error { panic(cause) }}
	testInterruptedUseRelease(t, protection, cause)
}

func testInterruptedUseRelease(t *testing.T, protection *interruptedUseRelease, cause error) {
	t.Helper()
	host, owner, use := acquireProtectedUse(t, protection)
	outcome, err := use.Close(testContext(t))
	if outcome.Released || err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("interrupted release lost its settlement conclusion: %+v, %v", outcome, err)
	}
	if cause != nil && !errors.Is(err, cause) {
		t.Fatalf("interrupted release lost its original cause: %v", err)
	}
	if err := host.Admission().RecoveryError(); !errors.Is(err, frame.ErrRecoveryRequired) {
		t.Fatalf("interrupted durable release left admission open: %v", err)
	}
	if protection.calls.Load() != 1 {
		t.Fatalf("release retried without a caller: %d", protection.calls.Load())
	}
	if outcome, err = use.Close(testContext(t)); err != nil || !outcome.Released || protection.calls.Load() != 2 {
		t.Fatalf("explicit close did not settle the original release: %+v, %v, calls=%d", outcome, err, protection.calls.Load())
	}
	if _, err := owner.Close(testContext(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Close(testContext(t)); err != nil {
		t.Fatal(err)
	}
}

func acquireProtectedUse(t *testing.T, protection frame.ExecutionProtection) (*frame.Host, *frame.Lifetime, *frame.Use) {
	t.Helper()
	host, err := frame.NewHost(context.Background(), frame.HostConfig{ExecutionProtection: protection})
	if err != nil {
		t.Fatal(err)
	}
	owner := frame.NewLifetime(context.Background())
	grant, err := host.Admission().OpenScope(testContext(t), "consumer")
	if err != nil {
		t.Fatal(err)
	}
	plugin := installNative(t, host, "provider", frame.Unit{
		Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo},
		Setup:       func(context.Context, *frame.Setup) ([]frame.ExportValue, error) { return nil, nil },
	})
	use, err := host.AcquireUnit(testContext(t), frame.UnitRef{InstallationID: "provider", Release: plugin.Revision(), UnitID: "main"}, frame.UseConfig{Owner: owner, Scope: grant, Realm: "realm"})
	if err != nil {
		t.Fatal(err)
	}
	return host, owner, use
}

type gatedUseRelease struct {
	entered chan struct{}
	result  chan error
	calls   atomic.Int64
}

func (*gatedUseRelease) ApproveUse(context.Context, frame.UseApproval) error { return nil }

func (p *gatedUseRelease) ReleaseUse(context.Context, string, frame.ReleaseProof) error {
	p.calls.Add(1)
	p.entered <- struct{}{}
	return <-p.result
}

func TestOverlappingUseClosesCannotPreauthorizeAnotherFailureRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		protection := &gatedUseRelease{entered: make(chan struct{}), result: make(chan error)}
		host, owner, use := acquireProtectedUse(t, protection)
		first, second := errors.New("first release failure"), errors.New("second release failure")
		use.BeginClose(frame.ErrClosed)
		<-protection.entered
		protection.result <- first
		synctest.Wait()

		type closeResult struct {
			outcome frame.CloseOutcome
			err     error
		}
		closed := make(chan closeResult, 2)
		closeUse := func() {
			outcome, err := use.Close(t.Context())
			closed <- closeResult{outcome, err}
		}
		go closeUse()
		<-protection.entered
		go closeUse()
		synctest.Wait()
		protection.result <- second
		synctest.Wait()
		if calls := protection.calls.Load(); calls != 2 {
			t.Errorf("overlapping closes authorized an unobserved failure's retry: calls=%d", calls)
		}
		for range 2 {
			result := <-closed
			if result.outcome.Released || !errors.Is(result.err, second) {
				t.Errorf("overlapping close lost the original failed attempt: %+v, %v", result.outcome, result.err)
			}
		}

		go closeUse()
		<-protection.entered
		protection.result <- nil
		result := <-closed
		if !result.outcome.Released || result.err != nil || protection.calls.Load() != 3 {
			t.Errorf("later explicit close did not settle the original release: %+v, %v, calls=%d", result.outcome, result.err, protection.calls.Load())
		}
		if _, err := owner.Close(t.Context()); err != nil {
			t.Error(err)
		}
		if _, err := host.Close(t.Context()); err != nil {
			t.Error(err)
		}
	})
}
