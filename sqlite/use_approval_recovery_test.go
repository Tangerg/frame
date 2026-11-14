package sqlite

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

func TestUseApprovalSettlementMatchesOriginalCommit(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "rolled back"
		if committed {
			name = "committed acknowledgement lost"
		}
		t.Run(name, func(t *testing.T) {
			store, _, admission := testStore(t)
			host, err := framehost.New(t.Context(), framehost.Config{Authority: store})
			if err != nil {
				t.Fatal(err)
			}
			contract, err := framehost.NewContract[string]("original.value", "1")
			if err != nil {
				t.Fatal(err)
			}
			if err := host.Register(contract); err != nil {
				t.Fatal(err)
			}
			plugin, err := framehost.Define(framehost.Definition{Revision: value.Hash([]byte("original")), Units: []framehost.Unit{{
				Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "value", Contract: contract.Ref()}}},
				Setup: func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) {
					return []framehost.ExportValue{framehost.Provide("value", contract, "original")}, nil
				},
			}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := host.Install(t.Context(), framehost.InstallationConfig{ID: "original-provider", Plugin: plugin, Enabled: true}); err != nil {
				t.Fatal(err)
			}
			const realm framehost.RealmID = "original-realm"
			root := framehost.ExportRef{InstallationID: "original-provider", Release: plugin.Revision(), UnitID: "main", Export: "value", Contract: contract.Ref()}
			if _, err := host.ActivateIn(t.Context(), realm, root); err != nil {
				t.Fatal(err)
			}
			plan, err := host.PlanWith(t.Context(), realm, nil, root)
			if err != nil {
				t.Fatal(err)
			}
			scope, err := admission.OpenScope(t.Context(), "original-scope")
			if err != nil {
				t.Fatal(err)
			}
			owner := frame.NewLifetime(context.Background())
			ackLost := errors.New("test: sqlite committed but acknowledgement was lost")
			var injected atomic.Bool
			inject := func(statement string) error {
				if statement == "COMMIT" && injected.CompareAndSwap(false, true) {
					return ackLost
				}
				return nil
			}
			fault := transactionFault{}
			if committed {
				fault.afterExec = inject
			} else {
				fault.beforeExec = inject
			}
			installTransactionFault(t, store, fault)
			use, acquireErr := host.Acquire(t.Context(), plan, framehost.UseConfig{Owner: owner, Scope: scope, Realm: realm, ExecutionID: "original-execution"})
			if use.Valid() || !errors.Is(acquireErr, ackLost) {
				t.Fatalf("fault did not affect the intended approval: use=%v err=%v", use, acquireErr)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			ownerOutcome, ownerErr := owner.Close(ctx)
			hostOutcome, hostErr := host.Close(ctx)
			records, err := store.Uses(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !ownerOutcome.Released || ownerErr != nil || !hostOutcome.Released || hostErr != nil {
				t.Fatalf("original settlement did not conclude: owner=%+v (%v), host=%+v (%v)", ownerOutcome, ownerErr, hostOutcome, hostErr)
			}
			if !committed {
				if len(records) != 0 {
					t.Fatalf("rolled back approval gained a durable record: %+v", records)
				}
				return
			}
			if len(records) != 1 || !records[0].Released || records[0].Proof == nil || records[0].Proof.Host == nil || records[0].Proof.Host.CloseID == 0 {
				t.Fatalf("committed approval lost its original settlement: %+v", records)
			}
		})
	}
}
