package host_test

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
)

func TestInterruptedUseApprovalKeepsOriginalSettlement(t *testing.T) {
	for _, interruption := range []string{"error", "panic", "goexit"} {
		t.Run(interruption, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				approvalFailure := errors.New("original approval acknowledgement lost")
				settlementFailure := errors.New("original approval settlement unavailable")
				protection := &reservedUseProtection{
					failures: 2,
					failure:  settlementFailure,
					releases: make(chan recordedUseRelease, 3),
					approve: func() error {
						switch interruption {
						case "panic":
							panic(approvalFailure)
						case "goexit":
							runtime.Goexit()
						}
						return approvalFailure
					},
				}
				host, err := framehost.New(t.Context(), framehost.Config{Authority: protectionAuthority(protection)})
				if err != nil {
					t.Fatal(err)
				}
				var cleanups atomic.Int64
				plugin := installNative(t, host, "provider", framehost.Unit{
					Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
					Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
						_, err := setup.Cleanup("original provider", func() error { cleanups.Add(1); return nil })
						return nil, err
					},
				})
				owner, scope := ownerAndScope(t, host)
				completed := make(chan struct{})
				go func() {
					defer close(completed)
					defer func() {
						recovered := recover()
						if interruption == "panic" && recovered != approvalFailure {
							t.Errorf("approval panic changed: %v", recovered)
						}
					}()
					use, err := host.AcquireUnit(t.Context(), framehost.UnitRef{InstallationID: "provider", Release: plugin.Revision(), UnitID: "main"}, framehost.UseConfig{Owner: owner, Scope: scope, Realm: "realm"})
					if interruption != "error" || use.Valid() || !errors.Is(err, approvalFailure) {
						t.Errorf("interrupted approval conclusion: use=%v, error=%v", use, err)
					}
				}()
				<-completed
				synctest.Wait()
				if protection.releaseCalls.Load() != 1 || cleanups.Load() != 0 {
					t.Fatalf("unknown approval released its original owner: settlements=%d, cleanups=%d", protection.releaseCalls.Load(), cleanups.Load())
				}
				if err := host.Admission().RecoveryError(); !errors.Is(err, framehost.ErrRecoveryRequired) {
					t.Fatalf("interrupted approval left admission open: %v", err)
				}
				outcome, err := host.Close(testContext(t))
				if outcome.Released || !errors.Is(err, settlementFailure) || cleanups.Load() != 0 || protection.releaseCalls.Load() != 2 {
					t.Fatalf("failed original settlement freed its provider: %+v, %v", outcome, err)
				}
				outcome, err = host.Close(testContext(t))
				if !outcome.Released || err != nil && !errors.Is(err, frame.ErrCloseFailed) || cleanups.Load() != 1 || protection.releaseCalls.Load() != 3 {
					t.Fatalf("explicit retry did not settle original approval: %+v, %v", outcome, err)
				}
				var original frame.ReleaseProof
				for range 3 {
					release := <-protection.releases
					if original.CloseID == 0 {
						original = release.proof
					}
					if release.id != protection.approval.ID || release.proof != original || original.CloseID == 0 {
						t.Errorf("settlement replaced original identity or proof: %+v", release)
					}
				}
				if protection.approvals.Load() != 1 {
					t.Errorf("settlement retried approval: %d", protection.approvals.Load())
				}
				if outcome, err := owner.Close(testContext(t)); !outcome.Released || err != nil && !errors.Is(err, frame.ErrCloseFailed) {
					t.Errorf("original owner remained pending: %+v, %v", outcome, err)
				}
			})
		})
	}
}
