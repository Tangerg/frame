package browser_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
)

func pendingBrowserPlan(e *bridgeEnvironment, id string) (browser.LocalPlan, <-chan error) {
	e.t.Helper()
	done := beginBrowserActivation(e, id)
	for {
		poll, err := e.bridge.Poll(e.ctx, e.credentials.Token)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, plan := range poll.Plans {
			if plan.Units[0].Instance.Source.InstallationID == frame.InstallationID(id) {
				return plan, done
			}
		}
		select {
		case err := <-done:
			e.t.Fatalf("activation ended before its plan: %v", err)
		case <-e.ctx.Done():
			e.t.Fatal(e.ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}

func TestPollKeepsUnadmittedExitWithHost(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true)
	plan, done := pendingBrowserPlan(e, "pending")
	for range 2 {
		poll, err := e.bridge.Poll(e.ctx, e.credentials.Token)
		if err != nil || len(poll.Plans) != 1 || poll.Plans[0].Execution != plan.Execution {
			t.Fatalf("lost or deferred poll changed the pending plan: %+v %v", poll, err)
		}
	}
	proof := executionProof(plan)
	if _, err := e.bridge.Prove(e.ctx, e.credentials.Token, proof); !errors.Is(err, frame.ErrStaleBinding) {
		t.Fatalf("unadmitted plan acquired a browser proof owner: %v", err)
	}
	if _, err := e.store.ExitProof(e.ctx, plan.Execution.ID, proof.ProofID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("refused proof changed durable evidence: %v", err)
	}
	if err := e.bridge.SealRealm(e.ctx, e.credentials.Token); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("sealed preparation succeeded")
	}
	if err := e.host.WaitInstanceRetired(e.ctx, plan.Execution.ID); err != nil {
		t.Fatal(err)
	}
	admission, err := e.bridge.AdmitPlan(e.ctx, e.credentials.Token, plan)
	if err != nil || admission.Decision != browser.PlanNotAdmitted || admission.Realm != plan.Realm || admission.Execution != plan.Execution {
		t.Fatalf("original non-admission was not recoverable: %+v %v", admission, err)
	}
	record, err := e.store.Attempt(e.ctx, plan.Execution.ID)
	if err != nil || !record.Released || record.Proof.Kind != "not-admitted" {
		t.Fatalf("poll transferred the Host's exit ownership: %+v %v", record, err)
	}
	if _, err := e.bridge.Prove(e.ctx, e.credentials.Token, proof); !errors.Is(err, frame.ErrStaleBinding) {
		t.Fatalf("settled non-admission accepted a competing proof: %v", err)
	}
}

func TestAdmissionRetainsOriginalBrowserExitAcrossLostReply(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true)
	plan, done := pendingBrowserPlan(e, "pending")
	e.admit(plan)
	if err := e.bridge.SealRealm(e.ctx, e.credentials.Token); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("sealed preparation succeeded")
	}
	record, err := e.store.Attempt(e.ctx, plan.Execution.ID)
	if err != nil || record.Released {
		t.Fatalf("Host replaced the admitted browser's proof: %+v %v", record, err)
	}
	admission, err := e.bridge.AdmitPlan(e.ctx, e.credentials.Token, plan)
	if err != nil || admission.Decision != browser.PlanCleanupRequired {
		t.Fatalf("lost admission reply lost the original obligation: %+v %v", admission, err)
	}
	poll, err := e.bridge.Poll(e.ctx, e.credentials.Token)
	if err != nil || len(poll.Plans) != 0 || len(poll.Fences) != 1 {
		t.Fatalf("admitted preparation did not retain its fence: %+v %v", poll, err)
	}
	if _, err := e.bridge.Prove(e.ctx, e.credentials.Token, executionProof(plan)); err != nil {
		t.Fatal(err)
	}
	admission, err = e.bridge.AdmitPlan(e.ctx, e.credentials.Token, plan)
	if err != nil || admission.Decision != browser.PlanSettled {
		t.Fatalf("settled execution was relabeled never admitted: %+v %v", admission, err)
	}
}

func TestReconnectBeforeFirstAdmissionPreservesTheRealm(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true)
	live := e.start("live")
	pending, done := pendingBrowserPlan(e, "pending")
	request := browser.ReconnectRequest{Authority: e.credentials.Authority, Realm: e.credentials.Realm, Executions: []browser.Execution{live.Execution, pending.Execution}}
	result, err := e.bridge.Reconnect(e.ctx, e.credentials.Token, request)
	if err != nil || result.Mode != browser.Revalidated || len(result.Executions) != 2 {
		t.Fatalf("pending local owner sealed an otherwise current Realm: %+v %v", result, err)
	}
	if _, err := e.bridge.Prove(e.ctx, e.credentials.Token, executionProof(pending)); !errors.Is(err, frame.ErrStaleBinding) {
		t.Fatalf("reconnect advanced the plan's admission: %v", err)
	}
	_, grant := e.acquire(live, "after-reconnect")
	if _, err := e.bridge.Prove(e.ctx, e.credentials.Token, useProof(live, grant)); err != nil {
		t.Fatal(err)
	}
	if err := e.bridge.SealRealm(e.ctx, e.credentials.Token); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("sealed preparation succeeded")
	}
	admission, err := e.bridge.AdmitPlan(e.ctx, e.credentials.Token, pending)
	if err != nil || admission.Decision != browser.PlanNotAdmitted {
		t.Fatalf("reconnect took the Host's original non-admission proof: %+v %v", admission, err)
	}
}

func TestAdmissionPreservesValidationFailureAndOriginalExitOwner(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true)
	plan, done := pendingBrowserPlan(e, "pending")
	failure := errors.New("recover the original authority")
	e.host.Admission().RequireRecovery(failure)
	admission, err := e.bridge.AdmitPlan(e.ctx, e.credentials.Token, plan)
	if !errors.Is(err, failure) || admission.Decision != "" {
		t.Fatalf("validation failure was replaced with a successful decision: %+v %v", admission, err)
	}
	admission, err = e.bridge.AdmitPlan(e.ctx, e.credentials.Token, plan)
	if err != nil || admission.Decision != browser.PlanCleanupRequired {
		t.Fatalf("failed validation lost the already reserved browser owner: %+v %v", admission, err)
	}
	e.plans = append(e.plans, plan)
	if _, err := e.bridge.Prove(e.ctx, e.credentials.Token, executionProof(plan)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("failed preparation succeeded")
	}
}

func TestUncommittedNonAdmissionKeepsPendingOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	e := openBridge(t, path, true)
	plan, done := pendingBrowserPlan(e, "pending")
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TRIGGER refuse_admission_exit BEFORE UPDATE OF state ON frame_attempts BEGIN SELECT RAISE(ABORT,'injected'); END"); err != nil {
		t.Fatal(err)
	}
	if err := e.bridge.SealRealm(e.ctx, e.credentials.Token); err == nil {
		t.Fatal("uncommitted non-admission was acknowledged")
	}
	if admission, err := e.bridge.AdmitPlan(e.ctx, e.credentials.Token, plan); err == nil || admission.Decision != "" {
		t.Fatalf("storage failure discarded the pending local owner: %+v %v", admission, err)
	}
	record, err := e.store.Attempt(e.ctx, plan.Execution.ID)
	if err != nil || record.Released {
		t.Fatalf("failed settlement changed original protection: %+v %v", record, err)
	}
	if _, err := db.Exec("DROP TRIGGER refuse_admission_exit"); err != nil {
		t.Fatal(err)
	}
	admission, err := e.bridge.AdmitPlan(e.ctx, e.credentials.Token, plan)
	if err != nil || admission.Decision != browser.PlanNotAdmitted {
		t.Fatalf("original non-admission did not settle after recovery: %+v %v", admission, err)
	}
	if err := <-done; err == nil {
		t.Fatal("sealed preparation succeeded")
	}
	if err := e.host.WaitInstanceRetired(e.ctx, plan.Execution.ID); err != nil {
		t.Fatal(err)
	}
}

func TestAdmissionAndRealmSealHaveOneExitOwner(t *testing.T) {
	for index := range 8 {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			e := openBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true)
			plan, done := pendingBrowserPlan(e, "pending")
			start := make(chan struct{})
			sealed := make(chan error, 1)
			go func() {
				<-start
				sealed <- e.bridge.SealRealm(e.ctx, e.credentials.Token)
			}()
			close(start)
			admission, err := e.bridge.AdmitPlan(e.ctx, e.credentials.Token, plan)
			if err != nil {
				t.Fatal(err)
			}
			if err := <-sealed; err != nil {
				t.Fatal(err)
			}
			record, err := e.store.Attempt(e.ctx, plan.Execution.ID)
			if err != nil {
				t.Fatal(err)
			}
			if record.Released {
				if admission.Decision != browser.PlanNotAdmitted || record.Proof.Kind != "not-admitted" {
					t.Fatalf("Host and browser both acquired exit ownership: %+v %+v", admission, record)
				}
			} else {
				if admission.Decision != browser.PlanAdmitted && admission.Decision != browser.PlanCleanupRequired {
					t.Fatalf("unsettled attempt lost its browser owner: %+v %+v", admission, record)
				}
				e.plans = append(e.plans, plan)
				if _, err := e.bridge.Prove(e.ctx, e.credentials.Token, executionProof(plan)); err != nil {
					t.Fatal(err)
				}
			}
			if err := <-done; err == nil {
				t.Fatal("sealed preparation succeeded")
			}
			if err := e.host.WaitInstanceRetired(e.ctx, plan.Execution.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPendingAdmissionRecoveryHelper(t *testing.T) {
	path := os.Getenv("FRAME_ADMISSION_CRASH_DB")
	if path == "" {
		return
	}
	e := openBridge(t, path, false)
	plan, _ := pendingBrowserPlan(e, "pending")
	switch os.Getenv("FRAME_ADMISSION_CRASH_STATE") {
	case "admitted":
		e.admit(plan)
	case "notAdmitted":
		if err := e.bridge.SealRealm(e.ctx, e.credentials.Token); err != nil {
			t.Fatal(err)
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(recoveryFixture{Credentials: e.credentials, Plan: plan}); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func TestReconstructedHostResolvesOriginalPendingAdmission(t *testing.T) {
	for _, state := range []string{"unresolved", "admitted", "notAdmitted"} {
		t.Run(state, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.sqlite")
			command := exec.Command(os.Args[0], "-test.run=^TestPendingAdmissionRecoveryHelper$")
			command.Env = append(os.Environ(), "FRAME_ADMISSION_CRASH_DB="+path, "FRAME_ADMISSION_CRASH_STATE="+state)
			output, err := command.Output()
			if err != nil {
				t.Fatalf("helper: %v %s", err, output)
			}
			var fixture recoveryFixture
			if err := json.Unmarshal(output, &fixture); err != nil {
				t.Fatal(err)
			}
			e := openBridge(t, path, true)
			credentials, err := e.bridge.ResumeRealm(e.ctx, fixture.Credentials.Realm, fixture.Credentials.ResumeToken)
			if err != nil {
				t.Fatal(err)
			}
			admission, err := e.bridge.AdmitPlan(e.ctx, credentials.Token, fixture.Plan)
			want := browser.PlanCleanupRequired
			if state == "notAdmitted" {
				want = browser.PlanNotAdmitted
			}
			if err != nil || admission.Decision != want || admission.Authority != credentials.Authority || admission.Authority == fixture.Plan.Authority || admission.Execution != fixture.Plan.Execution || admission.Realm != fixture.Plan.Realm {
				t.Fatalf("old attempt was recreated or relabeled: %+v %v", admission, err)
			}
			for _, change := range []func(*browser.LocalPlan){
				func(plan *browser.LocalPlan) { plan.Execution.Generation += "0" },
				func(plan *browser.LocalPlan) { plan.ScopeGeneration += "0" },
				func(plan *browser.LocalPlan) { plan.Authority.Epoch = credentials.Authority.Epoch },
			} {
				forged := fixture.Plan
				change(&forged)
				if _, err := e.bridge.AdmitPlan(e.ctx, credentials.Token, forged); !errors.Is(err, frame.ErrStaleBinding) {
					t.Fatalf("recovery accepted another original identity: %v", err)
				}
			}
			if want == browser.PlanCleanupRequired {
				if _, err := e.bridge.Prove(e.ctx, credentials.Token, executionProof(fixture.Plan)); err != nil {
					t.Fatal(err)
				}
				admission, err = e.bridge.AdmitPlan(e.ctx, credentials.Token, fixture.Plan)
				if err != nil || admission.Decision != browser.PlanSettled {
					t.Fatalf("original proof did not settle pending admission: %+v %v", admission, err)
				}
			}
			dependencies, err := e.store.ExecutionDependencies(e.ctx)
			if err != nil || len(dependencies) != 0 {
				t.Fatalf("resolved original attempt retained protection: %+v %v", dependencies, err)
			}
		})
	}
}
