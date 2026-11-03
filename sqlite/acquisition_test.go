package sqlite

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	modernc "modernc.org/sqlite"
)

type acquisitionBoundary string

const (
	beginAcquisition  acquisitionBoundary = "begin"
	policyAcquisition acquisitionBoundary = "read-policy"
)

func (b acquisitionBoundary) loseAcknowledgement(injected *atomic.Bool) transactionFault {
	if b == beginAcquisition {
		return transactionFault{afterExec: func(query string) error {
			if query == "BEGIN IMMEDIATE" && injected.CompareAndSwap(false, true) {
				return context.Canceled
			}
			return nil
		}}
	}
	return transactionFault{afterAuthorize: func(authorizer modernc.AuthorizerFn) error {
		if authorizer != nil && authorizer(modernc.AuthInsert, "", "", "", "") == modernc.AuthorizerDeny && injected.CompareAndSwap(false, true) {
			return context.Canceled
		}
		return nil
	}}
}

func TestAcquisitionAcknowledgementLossSettlesOriginalConnection(t *testing.T) {
	for _, boundary := range []acquisitionBoundary{beginAcquisition, policyAcquisition} {
		t.Run(string(boundary), func(t *testing.T) {
			store, owner, admission := testStore(t)
			var injected atomic.Bool
			installTransactionFault(t, store, boundary.loseAcknowledgement(&injected))
			var err error
			if boundary == beginAcquisition {
				_, err = owner.Prepare(t.Context(), []byte("interrupted acquisition"))
			} else {
				err = store.Read(t.Context(), func(Reader) error {
					t.Fatal("unacknowledged read scope entered its callback")
					return nil
				})
			}
			if !injected.Load() || !errors.Is(err, context.Canceled) {
				t.Fatalf("acquisition boundary not reached: injected=%t error=%v", injected.Load(), err)
			}
			id, err := owner.Prepare(t.Context(), []byte("next acquisition"))
			if err != nil || id.Sequence != 1 || admission.RecoveryError() != nil {
				t.Fatalf("connection returned before its state was restored: id=%+v error=%v recovery=%v", id, err, admission.RecoveryError())
			}
		})
	}
}

func TestUnconfirmedAcquisitionCleanupRetainsOriginalAuthority(t *testing.T) {
	for _, boundary := range []acquisitionBoundary{beginAcquisition, policyAcquisition} {
		t.Run(string(boundary), func(t *testing.T) {
			store, owner, admission := testStore(t)
			var injected, disposalAllowed atomic.Bool
			var opens, releases atomic.Int64
			fault := boundary.loseAcknowledgement(&injected)
			fault.opened = func() { opens.Add(1) }
			fault.beforeExec = func(query string) error {
				if query == "ROLLBACK" {
					return errors.New("original acquisition cleanup unconfirmed")
				}
				return nil
			}
			fault.beforeAuthorize = func(modernc.AuthorizerFn) error {
				if injected.Load() {
					return errors.New("original policy cleanup unconfirmed")
				}
				return nil
			}
			fault.beforeClose = func() error {
				if !disposalAllowed.Load() {
					return errors.New("original connection is still open")
				}
				return nil
			}
			fault.afterClose = func() { releases.Add(1) }
			path := installTransactionFault(t, store, fault)
			defer disposalAllowed.Store(true)
			var err error
			if boundary == beginAcquisition {
				_, err = owner.Prepare(t.Context(), []byte("unconfirmed acquisition"))
			} else {
				err = store.Read(t.Context(), func(Reader) error { t.Fatal("read callback entered"); return nil })
			}
			if !errors.Is(err, ErrIndeterminate) || !errors.Is(err, context.Canceled) || admission.RecoveryError() == nil {
				t.Fatalf("unconfirmed cleanup was not fenced: error=%v recovery=%v", err, admission.RecoveryError())
			}
			if lock, err := acquireAuthority(path); err == nil {
				_ = releaseAuthority(lock)
				t.Fatal("unconfirmed acquisition released the authority")
			}
			if opens.Load() != 1 || releases.Load() != 0 {
				t.Fatalf("unconfirmed cleanup replaced its connection: opens=%d releases=%d", opens.Load(), releases.Load())
			}
			disposalAllowed.Store(true)
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if opens.Load() != 1 || releases.Load() != 1 {
				t.Fatalf("cleanup did not settle the original connection: opens=%d releases=%d", opens.Load(), releases.Load())
			}
			lock, err := acquireAuthority(path)
			if err != nil {
				t.Fatalf("confirmed release retained authority: %v", err)
			}
			if err := releaseAuthority(lock); err != nil {
				t.Fatal(err)
			}
		})
	}
}
