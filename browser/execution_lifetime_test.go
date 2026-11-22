package browser

import (
	"context"
	"path/filepath"
	"testing"
	"testing/synctest"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

func TestExecutionRetirementJoinsItsOriginalApproval(t *testing.T) {
	for _, stored := range []bool{false, true} {
		name := "rolled back"
		if stored {
			name = "committed acknowledgement lost"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store, err := sqlite.Open(t.Context(), sqlite.Config{Path: filepath.Join(t.TempDir(), "state.sqlite"), Admission: framehost.NewAdmission()})
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := store.Close(); err != nil {
						t.Error(err)
					}
				}()
				realm := Realm{ID: "realm", Epoch: "1"}
				source := framehost.UnitRef{InstallationID: "original", Release: value.Hash(nil), UnitID: "view"}
				attempt := sqlite.Attempt{
					ID: "execution", Epoch: store.Epoch(), Realm: string(realm.HostRealm()), Profile: framehost.BrowserJS,
					Scope: "scope", ScopeGeneration: 1, Generation: 1, Source: source,
					Binding: framehost.BindingSnapshot{Roots: []framehost.ExportRef{}, UnitRoots: []framehost.UnitRef{source}, Units: []framehost.UnitBinding{{Source: source, Configuration: value.Hash(nil)}}},
				}
				if stored {
					if state, err := store.RecordAttempt(t.Context(), attempt); state != framehost.Committed || err != nil {
						t.Fatalf("original approval: %s, %v", state, err)
					}
				}
				bridge := &Bridge{
					config:     BridgeConfig{Store: store},
					executions: newBoundedTable[string, *browserExecution]("executions", 1),
					sessions:   newBoundedTable[framehost.RealmID, *realmSession]("realms", 1),
				}
				entry := &browserExecution{attempt: attempt, plan: LocalPlan{Realm: realm, Execution: Execution{ID: attempt.ID, Generation: "1"}}, approvalDone: make(chan struct{})}
				lifetime := frame.NewLifetime(context.Background())
				resource, err := lifetime.Own(frame.Resource{Name: "original execution", Close: func(ctx context.Context) (frame.ReleaseState, error) { return bridge.closeExecution(ctx, entry) }})
				if err != nil {
					t.Fatal(err)
				}
				entry.reserveExit(resource)
				if err := bridge.executions.reserve(); err != nil {
					t.Fatal(err)
				}
				bridge.executions.commit(attempt.ID, entry)
				entries, current := bridge.reportedExecutions(&realmSession{realm: realm}, ReconnectRequest{Authority: bridge.authority, Executions: []Execution{entry.plan.Execution}})
				if current || len(entries) != 0 {
					t.Fatal("pending approval authorized a reconnected execution")
				}
				closed := make(chan struct{})
				go func() {
					defer close(closed)
					if outcome, err := lifetime.Close(t.Context()); !outcome.Released || err != nil {
						t.Errorf("original execution did not retire: %+v, %v", outcome, err)
					}
				}()
				synctest.Wait()
				select {
				case <-closed:
					t.Fatal("retirement overtook the original approval")
				default:
				}
				bridge.mu.Lock()
				if bridge.executions.get(attempt.ID) != entry || !entry.isFenced() {
					t.Error("pending approval lost its original cleanup owner")
				}
				bridge.mu.Unlock()
				close(entry.approvalDone)
				<-closed
				if bridge.executions.get(attempt.ID) != nil {
					t.Error("concluded original execution retained its registration")
				}
				observer, cancel := context.WithCancel(t.Context())
				cancel()
				for range 32 {
					state, err := bridge.closeExecution(observer, entry)
					if state != frame.ResourceReleased || err != nil {
						t.Fatalf("observer cancellation replaced confirmed release: %s, %v", state, err)
					}
				}
				records, err := store.Attempts(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if stored {
					if len(records) != 1 || !isUnadmittedRecord(records[0]) {
						t.Fatalf("original non-admission proof did not settle: %+v", records)
					}
				} else if len(records) != 0 {
					t.Fatalf("rolled-back approval gained a replacement record: %+v", records)
				}
			})
		})
	}
}
