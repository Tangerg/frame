package browser_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Tangerg/frame/browser"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/sqlite"
)

func TestReadyBelongsToTheOriginalRuntimeAdmission(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "store.db"), true)
	plan, activated := pendingBrowserPlan(e, "pending-runtime")
	report := e.readyReport(plan)
	if result, err := e.bridge.Ready(e.ctx, e.credentials.Token, report); err != nil || result.Decision != browser.Rejected {
		t.Fatalf("Ready preceded Runtime admission: %+v, %v", result, err)
	}
	e.admit(plan)
	for range 2 {
		if result, err := e.bridge.Ready(e.ctx, e.credentials.Token, report); err != nil || result.Decision != browser.Accepted {
			t.Fatalf("original Ready was not accepted: %+v, %v", result, err)
		}
	}
	if err := <-activated; err != nil {
		t.Fatal(err)
	}
	report.OperationID = "replacement-ready"
	if _, err := e.bridge.Ready(e.ctx, e.credentials.Token, report); !errors.Is(err, sqlite.ErrConflict) {
		t.Fatalf("another Ready identity replaced the original result: %v", err)
	}
	if poll, err := e.bridge.Poll(e.ctx, e.credentials.Token); err != nil || len(poll.Plans) != 0 {
		t.Fatalf("accepted Ready returned to pending preparation: %+v, %v", poll, err)
	}
}

func TestReadyPreservesEachObjectAndItsExactMetadataOwner(t *testing.T) {
	tests := []struct {
		name   string
		change func(*browser.ReadyReport)
	}{
		{"invalid object identity", func(report *browser.ReadyReport) {
			report.Objects[0].ObjectID = "invalid object id"
			report.Contributions[0].Object = report.Objects[0]
		}},
		{"shared object identity", func(report *browser.ReadyReport) {
			report.Objects[1].ObjectID = report.Objects[0].ObjectID
			report.Contributions = report.Contributions[:1]
		}},
		{"relabelled metadata owner", func(report *browser.ReadyReport) {
			report.Contributions[0].Object.Export = report.Objects[1].Export
		}},
		{"orphan metadata", func(report *browser.ReadyReport) {
			orphan := report.Contributions[0]
			orphan.Object.ObjectID = "orphan"
			report.Contributions = append(report.Contributions, orphan)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e := openBridge(t, filepath.Join(t.TempDir(), "store.db"), true)
			plan, activated := e.beginActivation("publisher")
			report := e.readyReport(plan)
			test.change(&report)
			result, err := e.bridge.Ready(e.ctx, e.credentials.Token, report)
			if err != nil || result.Decision != browser.Rejected {
				t.Fatalf("inconsistent prepared identity was accepted: %+v %v", result, err)
			}
			if result, err := e.bridge.Ready(e.ctx, e.credentials.Token, e.readyReport(plan)); err != nil || result.Decision != browser.Accepted {
				t.Fatalf("valid report could not finish original preparation: %+v %v", result, err)
			}
			if err := <-activated; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReadyAcceptanceSurvivesRetirement(t *testing.T) {
	for _, revoke := range []struct {
		name string
		run  func(*bridgeEnvironment, browser.LocalPlan) error
	}{
		{"instance retirement", func(e *bridgeEnvironment, plan browser.LocalPlan) error {
			return e.host.RetireInstance(e.ctx, plan.Execution.ID)
		}},
		{"realm sealing", func(e *bridgeEnvironment, _ browser.LocalPlan) error {
			return e.bridge.SealRealm(e.ctx, e.credentials.Token)
		}},
	} {
		t.Run(revoke.name, func(t *testing.T) {
			e := openBridge(t, filepath.Join(t.TempDir(), "store.db"), true)
			plan := e.start("publisher")
			report := e.readyReport(plan)
			if err := revoke.run(e, plan); err != nil {
				t.Fatal(err)
			}
			for {
				poll, err := e.bridge.Poll(e.ctx, e.credentials.Token)
				if err != nil {
					t.Fatal(err)
				}
				if len(poll.Fences) != 0 {
					break
				}
				select {
				case <-e.ctx.Done():
					t.Fatal(e.ctx.Err())
				case <-time.After(time.Millisecond):
				}
			}
			for range 2 {
				if result, err := e.bridge.Ready(e.ctx, e.credentials.Token, report); err != nil || result.Decision != browser.Accepted {
					t.Fatalf("retirement replaced the accepted Ready result: %+v, %v", result, err)
				}
			}
			changed := e.readyReport(plan)
			changed.OperationID = "replacement-ready"
			if _, err := e.bridge.Ready(e.ctx, e.credentials.Token, changed); !errors.Is(err, sqlite.ErrConflict) {
				t.Fatalf("retirement permitted Ready relabelling: %v", err)
			}
			changed = e.readyReport(plan)
			changed.Contributions[0].MetadataJSON = `{"label":"replacement"}`
			if _, err := e.bridge.Ready(e.ctx, e.credentials.Token, changed); !errors.Is(err, sqlite.ErrConflict) {
				t.Fatalf("retirement permitted another Ready payload: %v", err)
			}
			snapshot, err := e.host.SnapshotIn(e.ctx, browser.RealmID(plan.Realm), e.first.Ref())
			if err != nil {
				t.Fatal(err)
			}
			candidates, err := framehost.Candidates(snapshot, e.first)
			if err != nil || len(candidates) != 0 {
				t.Fatalf("Ready replay republished retired exports: %+v, %v", candidates, err)
			}
		})
	}
}

func TestSealedRealmCannotAcceptItsFirstReadyReport(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "store.db"), true)
	plan, activated := e.beginActivation("publisher")
	if err := e.bridge.SealRealm(e.ctx, e.credentials.Token); err != nil {
		t.Fatal(err)
	}
	if result, err := e.bridge.Ready(e.ctx, e.credentials.Token, e.readyReport(plan)); err != nil || result.Decision != browser.Rejected {
		t.Fatalf("sealed Realm admitted a new Ready report: %+v, %v", result, err)
	}
	if err := <-activated; err == nil {
		t.Fatal("sealed preparation published its exports")
	}
	record, err := e.store.Attempt(e.ctx, plan.Execution.ID)
	if err != nil || record.ExitProven() {
		t.Fatalf("Ready refusal replaced the Runtime's exit obligation: %+v, %v", record, err)
	}
}
