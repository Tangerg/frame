package sqlite

import (
	"context"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

func TestRefusedUseApprovalDoesNotCreateSettlementOrRecovery(t *testing.T) {
	admission := framehost.NewAdmission()
	config := testConfig(filepath.Join(t.TempDir(), "state.sqlite"), admission)
	config.MaxRequestBytes = 512
	store, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	host, err := framehost.New(t.Context(), framehost.Config{Authority: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if outcome, err := host.Close(ctx); !outcome.Released || err != nil {
			t.Errorf("host close: %+v, %v", outcome, err)
		}
	})
	plugin, err := framehost.Define(framehost.Definition{Revision: value.Hash([]byte("approval refusal")), Units: []framehost.Unit{{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
		Setup:       func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) { return nil, nil },
	}}})
	if err != nil {
		t.Fatal(err)
	}
	installed, err := host.Install(t.Context(), framehost.InstallationConfig{ID: "provider", Plugin: plugin, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := admission.OpenScope(t.Context(), "caller")
	if err != nil {
		t.Fatal(err)
	}
	var settlementReads atomic.Int64
	installTransactionFault(t, store, transactionFault{queryRows: func(_ context.Context, query string, rows driver.Rows) driver.Rows {
		if strings.Contains(query, "frame_use_approvals") {
			settlementReads.Add(1)
		}
		return rows
	}})
	owner := frame.NewLifetime(context.Background())
	use, err := host.AcquireUnit(t.Context(), installed.Unit("main"), framehost.UseConfig{Owner: owner, Scope: scope, Realm: "realm"})
	if !errors.Is(err, frame.ErrCapacity) || use.Valid() {
		t.Fatalf("oversized approval was not refused: use=%v, err=%v", use.Valid(), err)
	}
	if outcome, err := owner.Close(t.Context()); !outcome.Released || err != nil {
		t.Fatalf("refused borrow did not retire: %+v, %v", outcome, err)
	}
	if settlementReads.Load() != 0 {
		t.Errorf("definite refusal required compensating settlement reads: %d", settlementReads.Load())
	}
	if err := admission.RecoveryError(); err != nil {
		t.Errorf("definite refusal fenced healthy admission: %v", err)
	}
	localOwner := frame.NewLifetime(context.Background())
	defer localOwner.Close(context.Background())
	local, err := host.AcquireUnit(t.Context(), installed.Unit("main"), framehost.UseConfig{Owner: localOwner, Scope: scope})
	if err != nil {
		t.Fatalf("definite refusal blocked subsequent local work: %v", err)
	}
	if outcome, err := local.Close(t.Context()); !outcome.Released || err != nil {
		t.Fatalf("local close: %+v, %v", outcome, err)
	}
}

func TestUseApprovalReportsOriginalTransactionConclusion(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		statement string
		after     bool
		state     framehost.CommitState
		stored    bool
	}{
		{name: "committed", state: framehost.Committed, stored: true},
		{name: "begin refused", statement: "BEGIN IMMEDIATE", state: framehost.NotCommitted},
		{name: "begin acknowledgement lost", statement: "BEGIN IMMEDIATE", after: true, state: framehost.NotCommitted},
		{name: "insert refused", statement: "INSERT INTO frame_use_approvals", state: framehost.NotCommitted},
		{name: "commit refused", statement: "COMMIT", state: framehost.NotCommitted},
		{name: "commit acknowledgement lost", statement: "COMMIT", after: true, state: framehost.CommitIndeterminate, stored: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			store, _, admission := testStore(t)
			attempt := protectedAttempt(t, store)
			approval := framehost.UseApproval{ID: "original-use", ExecutionID: attempt.ID, Realm: framehost.RealmID(attempt.Realm), Scope: "scope", ScopeGeneration: 1, Epoch: store.Epoch(), Binding: attempt.Binding}
			failure := errors.New("original approval statement failed")
			var injected atomic.Bool
			inject := func(statement string) error {
				if scenario.statement != "" && strings.HasPrefix(statement, scenario.statement) && injected.CompareAndSwap(false, true) {
					return failure
				}
				return nil
			}
			fault := transactionFault{beforeExec: inject}
			if scenario.after {
				fault = transactionFault{afterExec: inject}
			}
			installTransactionFault(t, store, fault)
			var state framehost.CommitState
			err := admission.Admit(t.Context(), func(ctx context.Context) error {
				var err error
				state, err = store.ApproveUse(ctx, approval)
				return err
			})
			if state != scenario.state || (scenario.statement != "" && !errors.Is(err, failure)) || (scenario.statement == "" && err != nil) {
				t.Fatalf("original commit conclusion=%s, error=%v; want %s", state, err, scenario.state)
			}
			if recovery := admission.RecoveryError(); (recovery != nil) != (state == framehost.CommitIndeterminate) {
				t.Errorf("recovery disagrees with original transaction evidence: state=%s recovery=%v", state, recovery)
			}
			records, err := store.Uses(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if (len(records) == 1) != scenario.stored || len(records) > 1 {
				t.Fatalf("original approval records=%+v, want stored=%v", records, scenario.stored)
			}
			if scenario.stored && records[0].Approval.ID != approval.ID {
				t.Fatalf("commit changed the original approval: %+v", records[0])
			}
		})
	}
}

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
			var approvalReads atomic.Int64
			fault.queryRows = func(_ context.Context, query string, rows driver.Rows) driver.Rows {
				if strings.Contains(query, "frame_use_approvals") {
					approvalReads.Add(1)
				}
				return rows
			}
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
			reads := int64(1)
			if committed {
				reads++
			}
			if approvalReads.Load() != reads {
				t.Errorf("approval required %d reads, want %d", approvalReads.Load(), reads)
			}
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
			if len(records) != 1 || !records[0].Released() || records[0].Proof == nil || records[0].Proof.Host == nil || records[0].Proof.Host.CloseID == 0 {
				t.Fatalf("committed approval lost its original settlement: %+v", records)
			}
		})
	}
}
