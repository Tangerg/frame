package sqlite

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

func TestAcquisitionAcknowledgementLossSettlesOriginalConnection(t *testing.T) {
	for _, boundary := range []string{"BEGIN IMMEDIATE", "PRAGMA query_only=ON"} {
		t.Run(boundary, func(t *testing.T) {
			store, owner, admission := testStore(t)
			var injected atomic.Bool
			installTransactionFault(t, store, transactionFault{afterExec: func(query string) error {
				if query == boundary && injected.CompareAndSwap(false, true) {
					return context.Canceled
				}
				return nil
			}})
			var err error
			if boundary == "BEGIN IMMEDIATE" {
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
	for _, boundary := range []string{"BEGIN IMMEDIATE", "PRAGMA query_only=ON"} {
		t.Run(boundary, func(t *testing.T) {
			store, owner, admission := testStore(t)
			var injected, disposalAllowed atomic.Bool
			var opens, releases atomic.Int64
			path := installTransactionFault(t, store, transactionFault{
				opened: func() { opens.Add(1) },
				afterExec: func(query string) error {
					if query == boundary && injected.CompareAndSwap(false, true) {
						return context.Canceled
					}
					return nil
				},
				beforeExec: func(query string) error {
					if query == "ROLLBACK" || query == "PRAGMA query_only=OFF" {
						return errors.New("original acquisition cleanup unconfirmed")
					}
					return nil
				},
				beforeClose: func() error {
					if !disposalAllowed.Load() {
						return errors.New("original connection is still open")
					}
					return nil
				},
				afterClose: func() { releases.Add(1) },
			})
			defer disposalAllowed.Store(true)
			var err error
			if boundary == "BEGIN IMMEDIATE" {
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
