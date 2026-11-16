package host_test

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"weak"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
)

type interruptedUseRelease struct {
	release func() error
	calls   atomic.Int64
}

func (*interruptedUseRelease) ApproveUse(context.Context, framehost.UseApproval) (framehost.CommitState, error) {
	return framehost.Committed, nil
}

func (p *interruptedUseRelease) SettleUse(context.Context, string, frame.ReleaseProof) error {
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
	if err := host.Admission().RecoveryError(); !errors.Is(err, framehost.ErrRecoveryRequired) {
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

func acquireProtectedUse(t *testing.T, protection framehost.ExecutionProtection) (framehost.Host, frame.Lifetime, framehost.Use) {
	t.Helper()
	host, err := framehost.New(context.Background(), framehost.Config{Authority: protectionAuthority(protection)})
	if err != nil {
		t.Fatal(err)
	}
	owner := frame.NewLifetime(context.Background())
	grant, err := host.Admission().OpenScope(testContext(t), "consumer")
	if err != nil {
		t.Fatal(err)
	}
	plugin := installNative(t, host, "provider", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
		Setup:       func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) { return nil, nil },
	})
	use, err := host.AcquireUnit(testContext(t), framehost.UnitRef{InstallationID: "provider", Release: plugin.Revision(), UnitID: "main"}, framehost.UseConfig{Owner: owner, Scope: grant, Realm: "realm"})
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

func TestUseSettlementRetryPreservesOriginalFailureIdentities(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		protection := &gatedUseRelease{entered: make(chan struct{}), result: make(chan error)}
		host, owner, use := acquireProtectedUse(t, protection)
		original := errors.New("original resource failure")
		settlement := errors.New("original settlement failure")
		if _, err := use.Lifetime().Own(frame.Resource{Name: "original execution", Close: func(context.Context) (frame.ReleaseState, error) {
			return frame.ResourceReleased, original
		}}); err != nil {
			t.Fatal(err)
		}
		causes := owner.ObserveCloseCauses()
		closing := owner.BeginClose(frame.ErrClosed)
		<-protection.entered
		protection.result <- settlement
		first, err := closing.Wait(t.Context())
		if first.Released || !errors.Is(err, frame.ErrUnconfirmedRelease) || !errors.Is(causes.Err(), original) || !errors.Is(causes.Err(), settlement) {
			t.Errorf("failed settlement lost original exit or failure facts: outcome=%+v error=%v causes=%v", first, err, causes.Err())
		}
		type closeResult struct {
			outcome frame.CloseOutcome
			err     error
		}
		closed := make(chan closeResult, 1)
		go func() { outcome, err := use.Close(t.Context()); closed <- closeResult{outcome, err} }()
		<-protection.entered
		protection.result <- nil
		result := <-closed
		if !result.outcome.Released || !errors.Is(result.err, original) {
			t.Errorf("retry replaced original resource failure: outcome=%+v error=%v", result.outcome, result.err)
		}
		last, err := closing.WaitReleased(t.Context())
		if !last.Released || last.ID != first.ID || !errors.Is(err, frame.ErrCloseFailed) {
			t.Errorf("retry replaced original parent close: outcome=%+v error=%v", last, err)
		}
		for _, cause := range []error{original, settlement} {
			if !errors.Is(causes.Err(), cause) || strings.Count(causes.Err().Error(), cause.Error()) != 1 {
				t.Errorf("parent lost or duplicated an original failure after retry: cause=%v report=%v", cause, causes.Err())
			}
		}
		if outcome, err := host.Close(t.Context()); !outcome.Released || !errors.Is(err, frame.ErrCloseFailed) {
			t.Errorf("Host lost its bounded close conclusion: outcome=%+v error=%v", outcome, err)
		}
	})
}

func retiredUseFailurePayload(t *testing.T) (framehost.Use, weak.Pointer[retainedPayload]) {
	t.Helper()
	protection := &interruptedUseRelease{release: func() error { return errors.New("settlement unavailable") }}
	host, owner, use := acquireProtectedUse(t, protection)
	payload := new(retainedPayload)
	original := &cleanupCause{payload: payload}
	if _, err := use.Lifetime().Own(frame.Resource{Name: "original failure payload", Close: func(context.Context) (frame.ReleaseState, error) {
		return frame.ResourceReleased, original
	}}); err != nil {
		t.Fatal(err)
	}
	if outcome, err := use.Close(testContext(t)); outcome.Released || !errors.Is(err, original) {
		t.Fatalf("failed settlement lost its original failure: outcome=%+v error=%v", outcome, err)
	}
	if outcome, err := use.Close(testContext(t)); !outcome.Released || !errors.Is(err, original) {
		t.Fatalf("retry lost its original failure: outcome=%+v error=%v", outcome, err)
	}
	if outcome, err := owner.Close(testContext(t)); !outcome.Released || !errors.Is(err, frame.ErrCloseFailed) {
		t.Fatalf("owner lost its bounded conclusion: outcome=%+v error=%v", outcome, err)
	}
	if outcome, err := host.Close(testContext(t)); !outcome.Released || !errors.Is(err, frame.ErrCloseFailed) {
		t.Fatalf("Host lost its bounded conclusion: outcome=%+v error=%v", outcome, err)
	}
	return use, weak.Make(payload)
}

func TestRetiredUseDropsOriginalFailureObservation(t *testing.T) {
	use, payload := retiredUseFailurePayload(t)
	awaitCollected(t, payload)
	if outcome, err := use.Close(testContext(t)); !outcome.Released || !errors.Is(err, frame.ErrCloseFailed) {
		t.Fatalf("terminal Use lost its bounded conclusion: outcome=%+v error=%v", outcome, err)
	}
	runtime.KeepAlive(use)
}

func (*gatedUseRelease) ApproveUse(context.Context, framehost.UseApproval) (framehost.CommitState, error) {
	return framehost.Committed, nil
}

func (p *gatedUseRelease) SettleUse(context.Context, string, frame.ReleaseProof) error {
	p.calls.Add(1)
	p.entered <- struct{}{}
	return <-p.result
}

func TestOverlappingUseClosesCannotPreauthorizeAnotherFailureRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		protection := &gatedUseRelease{entered: make(chan struct{}), result: make(chan error)}
		host, owner, use := acquireProtectedUse(t, protection)
		first, second := errors.New("first release failure"), errors.New("second release failure")
		use.Lifetime().BeginClose(frame.ErrClosed)
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
