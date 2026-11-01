package browser_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

type childRecoveryProvider struct {
	Plan  browser.LocalPlan `json:"plan"`
	Grant browser.UseGrant  `json:"grant"`
}

type childRecoveryFixture struct {
	Parent    recoveryFixture         `json:"parent"`
	Providers []childRecoveryProvider `json:"providers"`
	Unrelated childRecoveryProvider   `json:"unrelated"`
}

func TestBridgeChildRecoveryHelper(t *testing.T) {
	path := os.Getenv("FRAME_BRIDGE_CHILD_CRASH_DB")
	if path == "" {
		return
	}
	e := openBridge(t, path, false)
	parentPlan := e.start("parent")
	_, parent := e.acquire(parentPlan, "parent-use")
	fixture := childRecoveryFixture{Parent: recoveryFixture{Credentials: e.credentials, Plan: parentPlan, Grant: parent}}
	for _, id := range []string{"provider-a", "provider-b"} {
		plan := e.start(id)
		root := frame.ExportRef{InstallationID: frame.InstallationID(id), Release: e.plugin.Revision(), UnitID: "view", Export: "preview", Contract: e.first.Ref()}
		binding, err := e.host.PlanWith(e.ctx, browser.RealmID(e.credentials.Realm), nil, root)
		if err != nil {
			t.Fatal(err)
		}
		child, err := e.bridge.AcquirePlan(e.ctx, e.credentials.Token, "child:"+id, binding, parent.ID)
		if err != nil {
			t.Fatal(err)
		}
		fixture.Providers = append(fixture.Providers, childRecoveryProvider{Plan: plan, Grant: child})
	}
	otherPlan := e.start("unrelated")
	_, other := e.acquire(otherPlan, "unrelated-use")
	fixture.Unrelated = childRecoveryProvider{Plan: otherPlan, Grant: other}
	if err := json.NewEncoder(os.Stdout).Encode(fixture); err != nil {
		t.Fatal(err)
	}
	// The parent kills the actual process after it receives the durable grants.
	// No Host, Store, Use or execution cleanup runs before the crash.
	_, _ = io.Copy(io.Discard, os.Stdin)
	t.Fatal("parent ended helper input instead of killing the original Host")
}

func crashBridgeWithChildren(t *testing.T, path string) childRecoveryFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBridgeChildRecoveryHelper$")
	command.Env = append(os.Environ(), "FRAME_BRIDGE_CHILD_CRASH_DB="+path)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	var fixture childRecoveryFixture
	if err := json.NewDecoder(output).Decode(&fixture); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		stopped = true
		t.Fatalf("receive original child grants: %v\n%s", err, stderr.String())
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err = command.Wait()
	stopped = true
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.Success() {
		t.Fatalf("original Host did not terminate abruptly: %v\n%s", err, stderr.String())
	}
	return fixture
}

func TestChildDependencyOutsideStartAttemptSurvivesHostProcessCrash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	fixture := crashBridgeWithChildren(t, path)
	e := openBridge(t, path, false)
	t.Cleanup(func() {
		if err := e.store.Close(); err != nil {
			t.Error(err)
		}
	})
	records, err := e.store.RecoverInstallations(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.host.Restore(e.ctx, records, []*frame.Plugin{e.plugin}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.ConfirmRestoredInstallations(e.ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.host.Observations()) != 0 {
		t.Fatal("restoration created new executions for surviving browser objects")
	}
	attempts, err := e.store.Attempts(e.ctx)
	if err != nil || len(attempts) != 4 {
		t.Fatalf("original execution attempts: %+v %v", attempts, err)
	}
	for _, record := range attempts {
		if record.Attempt.ID == fixture.Parent.Plan.Execution.ID {
			dependencies := record.Attempt.Binding.Dependencies()
			if len(dependencies) != 1 || dependencies[0].InstallationID != "parent" {
				t.Fatalf("fixture already included child providers in StartAttempt: %+v", dependencies)
			}
		}
	}
	readUses := func() map[string]sqlite.UseRecord {
		t.Helper()
		uses, err := e.store.Uses(e.ctx)
		if err != nil || len(uses) != 4 {
			t.Fatalf("original durable uses: %+v %v", uses, err)
		}
		result := make(map[string]sqlite.UseRecord, len(uses))
		for _, use := range uses {
			result[use.Approval.ID] = use
		}
		return result
	}
	uses := readUses()
	for _, provider := range fixture.Providers {
		child, exists := uses[provider.Grant.ID]
		if !exists || child.Released || child.Approval.ParentID != fixture.Parent.Grant.ID || child.Approval.ExecutionID != fixture.Parent.Plan.Execution.ID {
			t.Fatalf("crash lost the original delegated child owner: %+v", child)
		}
		dependencies := child.Approval.Binding.Dependencies()
		if len(dependencies) != 1 || dependencies[0].InstallationID != provider.Plan.Units[0].Instance.Source.InstallationID || dependencies[0].Release != e.plugin.Revision() {
			t.Fatalf("child did not retain its additional exact provider: %+v", dependencies)
		}
	}
	assertDependencies := func(expected ...frame.InstallationID) {
		t.Helper()
		dependencies, err := e.store.ExecutionDependencies(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		actual := make([]frame.InstallationID, 0, len(dependencies))
		for _, dependency := range dependencies {
			if dependency.Release != e.plugin.Revision() {
				t.Fatalf("retained provider release changed: %+v", dependency)
			}
			actual = append(actual, dependency.InstallationID)
		}
		slices.Sort(actual)
		slices.Sort(expected)
		if !slices.Equal(actual, expected) {
			t.Fatalf("exact surviving dependency range: got %v, want %v", actual, expected)
		}
	}
	assertDependencies("parent", "provider-a", "provider-b", "unrelated")
	credentials, err := e.bridge.ResumeRealm(e.ctx, fixture.Parent.Credentials.Realm, fixture.Parent.Credentials.ResumeToken)
	if err != nil {
		t.Fatal(err)
	}
	result, err := e.bridge.Reconnect(e.ctx, credentials.Token, browser.ReconnectRequest{Authority: fixture.Parent.Credentials.Authority, Realm: credentials.Realm, Executions: []browser.Execution{fixture.Parent.Plan.Execution}})
	if err != nil || result.Mode != browser.CleanupOnly || credentials.Authority.Epoch == fixture.Parent.Credentials.Authority.Epoch {
		t.Fatalf("old Realm did not enter cleanup-only in the new epoch: %+v %v", result, err)
	}
	if _, err := e.bridge.Acquire(e.ctx, credentials.Token, browser.AcquireRequest{OperationID: "after-crash", ParentUseID: fixture.Parent.Grant.ID}); !errors.Is(err, frame.ErrFenced) {
		t.Fatalf("old parent admitted new work after reconstruction: %v", err)
	}
	replacement, err := frame.Define(frame.Definition{Revision: value.Hash([]byte("replacement-browser-plugin")), Units: []frame.Unit{{Declaration: e.plugin.Units()[0]}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.host.Replace(e.ctx, "provider-a", replacement, nil); !errors.Is(err, frame.ErrRecoveryRequired) {
		t.Fatalf("replacement crossed unresolved original child protection: %v", err)
	}
	prove := func(proof browser.CloseProof) {
		t.Helper()
		ack, err := e.bridge.Prove(e.ctx, credentials.Token, proof)
		if err != nil || !ack.Recorded {
			t.Fatalf("original exit was not durably acknowledged: %+v %v", ack, err)
		}
	}
	first := fixture.Providers[0]
	second := fixture.Providers[1]
	wrong := useProof(fixture.Parent.Plan, first.Grant)
	wrong.UseID = second.Grant.ID
	if _, err := e.bridge.Prove(e.ctx, credentials.Token, wrong); !errors.Is(err, frame.ErrStaleBinding) {
		t.Fatalf("another child's object range released the original child: %v", err)
	}
	prove(useProof(fixture.Parent.Plan, first.Grant))
	uses = readUses()
	if !uses[first.Grant.ID].Released || uses[second.Grant.ID].Released || uses[fixture.Parent.Grant.ID].Released || uses[fixture.Unrelated.Grant.ID].Released {
		t.Fatalf("child proof released another owner: %+v", uses)
	}
	prove(executionProof(first.Plan))
	assertDependencies("parent", "provider-b", "unrelated")
	parentProof := useProof(fixture.Parent.Plan, fixture.Parent.Grant)
	prove(parentProof)
	prove(parentProof) // Lost acknowledgement reuses the same original evidence.
	uses = readUses()
	if !uses[fixture.Parent.Grant.ID].Released || !uses[second.Grant.ID].Released || uses[fixture.Unrelated.Grant.ID].Released {
		t.Fatalf("parent proof failed to settle exactly its delegated closure: %+v", uses)
	}
	attempts, err = e.store.Attempts(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range attempts {
		if record.Attempt.ID != first.Plan.Execution.ID && record.Released {
			t.Fatalf("a subordinate Use proof released an independent execution: %+v", record)
		}
	}
	prove(executionProof(second.Plan))
	prove(executionProof(fixture.Parent.Plan))
	assertDependencies("unrelated")
	if err := e.host.Replace(e.ctx, "provider-a", replacement, nil); !errors.Is(err, frame.ErrRecoveryRequired) {
		t.Fatalf("partial cleanup reopened global recovery before all owners settled: %v", err)
	}
	prove(executionProof(fixture.Unrelated.Plan))
	assertDependencies()
	if err := e.host.Admission().Recover(e.ctx, func() error {
		dependencies, err := e.store.ExecutionDependencies(e.ctx)
		if err != nil {
			return err
		}
		if len(dependencies) != 0 {
			return frame.ErrRecoveryRequired
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.host.Replace(e.ctx, "provider-a", replacement, nil); err != nil {
		t.Fatalf("confirmed original exits did not unblock replacement: %v", err)
	}
	if outcome, err := e.host.Close(e.ctx); err != nil || !outcome.Released {
		t.Fatalf("close reconstructed Host: %+v %v", outcome, err)
	}
}
