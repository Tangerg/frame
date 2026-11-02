package browser_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

type testMetadata struct {
	Label string `json:"label"`
}
type testModules struct{}

func (testModules) Module(context.Context, frame.UnitBinding) (browser.Artifact, error) {
	return browser.Artifact{Path: "/plugins/test.js", Digest: value.Hash([]byte("test-module"))}, nil
}

type bridgeEnvironment struct {
	t           *testing.T
	ctx         context.Context
	store       *sqlite.Store
	host        *frame.Host
	bridge      *browser.Bridge
	credentials browser.Credentials
	first       frame.Point[testMetadata, browser.Object]
	second      frame.Point[testMetadata, browser.Object]
	plans       []browser.LocalPlan
	plugin      *frame.Plugin
}

func openBridge(t *testing.T, path string, cleanup bool) *bridgeEnvironment {
	return openConfiguredBridge(t, path, cleanup, nil)
}

func openConfiguredBridge(t *testing.T, path string, cleanup bool, configure func(*browser.BridgeConfig)) *bridgeEnvironment {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	admission := frame.NewAdmission()
	store, err := sqlite.Open(ctx, sqlite.Config{Path: path, Admission: admission})
	if err != nil {
		t.Fatal(err)
	}
	host, err := frame.NewHost(context.Background(), frame.HostConfig{Admission: admission, Epoch: store.Epoch(), Installations: store, ExecutionProtection: store})
	if err != nil {
		t.Fatal(err)
	}
	first, err := frame.NewPoint[testMetadata, browser.Object]("test.preview", "1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := frame.NewPoint[testMetadata, browser.Object]("test.theme", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(first, second); err != nil {
		t.Fatal(err)
	}
	config := browser.BridgeConfig{Host: host, Store: store, Source: testModules{}, Points: []frame.ContractRef{first.Ref(), second.Ref()}}
	if configure != nil {
		configure(&config)
	}
	bridge, err := browser.NewBridge(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.RegisterDriver(bridge); err != nil {
		t.Fatal(err)
	}
	plugin, err := frame.Define(frame.Definition{Revision: value.Hash([]byte("test-browser-plugin")), Units: []frame.Unit{{Declaration: frame.UnitDeclaration{ID: "view", Profile: frame.BrowserJS, Exports: []frame.ExportDeclaration{{Name: "preview", Contract: first.Ref()}, {Name: "theme", Contract: second.Ref()}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	e := &bridgeEnvironment{t: t, ctx: ctx, store: store, host: host, bridge: bridge, first: first, second: second, plugin: plugin}
	if cleanup {
		t.Cleanup(func() {
			for _, plan := range e.plans {
				ack, err := bridge.Prove(e.ctx, e.credentials.Token, executionProof(plan))
				if err != nil || !ack.Recorded {
					t.Errorf("execution proof: %+v %v", ack, err)
					continue
				}
				if err := host.WaitInstanceRetired(e.ctx, plan.Execution.ID); err != nil {
					t.Errorf("join proven instance: %v", err)
				}
			}
			closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			outcome, err := host.Close(closeCtx)
			if !outcome.Released {
				t.Errorf("Host did not release: %+v %v", outcome, err)
			}
			if err := store.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	return e
}

func (e *bridgeEnvironment) start(id string) browser.LocalPlan {
	e.t.Helper()
	if e.credentials.Token == "" {
		credentials, err := e.bridge.OpenRealm(e.ctx, "open-test", "0123456789abcdef0123456789abcdef")
		if err != nil {
			e.t.Fatal(err)
		}
		e.credentials = credentials
	}
	if _, err := e.host.Install(e.ctx, frame.InstallationConfig{ID: frame.InstallationID(id), Plugin: e.plugin, Configuration: []byte(`{"privateCredential":"must-not-reach-js"}`), Enabled: true}); err != nil {
		e.t.Fatal(err)
	}
	root := frame.ExportRef{InstallationID: frame.InstallationID(id), Release: e.plugin.Revision(), UnitID: "view", Export: "preview", Contract: e.first.Ref()}
	done := make(chan error, 1)
	go func() {
		_, err := e.host.ActivateIn(e.ctx, browser.RealmID(e.credentials.Realm), root)
		done <- err
	}()
	var plan browser.LocalPlan
	for plan.Execution.ID == "" {
		projection, err := e.bridge.Poll(e.ctx, e.credentials.Token)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, pending := range projection.Plans {
			if pending.Units[0].Instance.Source.InstallationID == frame.InstallationID(id) {
				plan = pending
				break
			}
		}
		if plan.Execution.ID != "" {
			break
		}
		select {
		case err := <-done:
			e.t.Fatalf("activation ended before plan: %v", err)
		case <-e.ctx.Done():
			e.t.Fatal(e.ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	e.plans = append(e.plans, plan)
	objects := []browser.ObjectRef{}
	contributions := []browser.PreparedContribution{}
	for _, declaration := range plan.Units[0].Exports {
		object := browser.ObjectRef{Export: browser.LocalExport{InstanceID: plan.Units[0].Instance.ID, Name: declaration.Name, Contract: declaration.Contract}, ObjectID: id + ":" + declaration.Name}
		objects = append(objects, object)
		contributions = append(contributions, browser.PreparedContribution{Object: object, MetadataJSON: `{"label":"` + id + `"}`})
	}
	report := browser.ReadyReport{OperationID: "ready:" + id, Authority: plan.Authority, Realm: plan.Realm, Execution: plan.Execution, Objects: objects, Contributions: contributions}
	result, err := e.bridge.Ready(e.ctx, e.credentials.Token, report)
	if err != nil || result.Decision != browser.Accepted {
		e.t.Fatalf("ready: %+v %v", result, err)
	}
	if err := <-done; err != nil {
		e.t.Fatal(err)
	}
	return plan
}

func (e *bridgeEnvironment) selectionRequest(plan browser.LocalPlan, operation string) browser.AcquireRequest {
	e.t.Helper()
	snapshot, err := e.bridge.Snapshot(e.ctx, e.credentials.Token, []string{e.first.Ref().Name, e.second.Ref().Name})
	if err != nil {
		e.t.Fatal(err)
	}
	request := browser.AcquireRequest{OperationID: operation, SnapshotID: snapshot.ID, Execution: plan.Execution, Selections: []browser.Selection{}}
	for _, point := range snapshot.Points {
		tokens := []string{}
		for _, candidate := range point.Candidates {
			if candidate.Source.InstallationID == plan.Units[0].Instance.Source.InstallationID {
				tokens = append(tokens, candidate.Token)
			}
		}
		request.Selections = append(request.Selections, browser.Selection{Point: point.Point, Candidates: tokens, Rule: "test/all/1"})
	}
	return request
}

func (e *bridgeEnvironment) acquire(plan browser.LocalPlan, operation string) (browser.AcquireRequest, browser.UseGrant) {
	e.t.Helper()
	request := e.selectionRequest(plan, operation)
	grant, err := e.bridge.Acquire(e.ctx, e.credentials.Token, request)
	if err != nil {
		e.t.Fatal(err)
	}
	if grant.OperationID != request.OperationID || grant.ParentUseID != request.ParentUseID {
		e.t.Fatalf("grant lost its original operation or parent: %+v", grant)
	}
	return request, grant
}

func executionProof(plan browser.LocalPlan) browser.CloseProof {
	return browser.CloseProof{CloseID: "close:" + plan.Execution.ID, ProofID: "proof:" + plan.Execution.ID, Authority: plan.Authority, Realm: plan.Realm, Execution: plan.Execution, ScopeID: plan.ScopeID, ScopeGeneration: plan.ScopeGeneration, ScopeKind: browser.ExecutionScope, Instances: []string{plan.Units[0].Instance.ID}, Status: browser.Released}
}
func useProof(plan browser.LocalPlan, grant browser.UseGrant) browser.CloseProof {
	proof := executionProof(plan)
	proof.CloseID = "close:" + grant.ID
	proof.ProofID = "proof:" + grant.ID
	proof.ScopeKind = browser.UseScope
	proof.UseID = grant.ID
	proof.Instances = []string{}
	for _, object := range grant.Objects {
		if !slices.Contains(proof.Instances, object.Export.InstanceID) {
			proof.Instances = append(proof.Instances, object.Export.InstanceID)
		}
	}
	return proof
}

func TestBridgeExactPlanCaptureUseAndProof(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "store.db"), true)
	plan := e.start("first")
	if plan.Units[0].ConfigurationJSON != "{}" {
		t.Fatal("opaque installation configuration was sent to the Realm")
	}
	if plan.Units[0].Instance.Configuration != value.Hash([]byte(`{"privateCredential":"must-not-reach-js"}`)) {
		t.Fatal("public projection replaced the original configuration identity")
	}
	forged := plan
	forged.ApprovalToken = "not-the-original"
	if err := e.bridge.ValidatePlan(e.ctx, e.credentials.Token, forged); !errors.Is(err, frame.ErrStaleBinding) {
		t.Fatalf("forged plan accepted: %v", err)
	}
	request, grant := e.acquire(plan, "first-use")
	if len(grant.Objects) != 2 {
		t.Fatalf("multi-Point capture lost objects: %+v", grant)
	}
	repeated, err := e.bridge.Acquire(e.ctx, e.credentials.Token, request)
	if err != nil || repeated.ID != grant.ID {
		t.Fatalf("lost ACK created another Use: %+v %v", repeated, err)
	}
	uses, err := e.store.Uses(e.ctx)
	if err != nil || len(uses) != 1 {
		t.Fatalf("durable approvals: %+v %v", uses, err)
	}
	other, err := e.bridge.OpenRealm(e.ctx, "open-other", "abcdef0123456789abcdef0123456789")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.bridge.Acquire(e.ctx, other.Token, request); !errors.Is(err, sqlite.ErrConflict) {
		t.Fatalf("other Realm observed original grant: %v", err)
	}
	proof := useProof(plan, grant)
	wrong := proof
	wrong.ScopeGeneration = "999"
	if _, err := e.bridge.Prove(e.ctx, e.credentials.Token, wrong); !errors.Is(err, frame.ErrStaleBinding) {
		t.Fatalf("wrong generation released Use: %v", err)
	}
	ack, err := e.bridge.Prove(e.ctx, e.credentials.Token, proof)
	if err != nil || !ack.Recorded {
		t.Fatalf("proof: %+v %v", ack, err)
	}
	if err := e.bridge.ValidateUse(e.ctx, e.credentials.Token, grant); err == nil {
		t.Fatal("released Use accepted")
	}
	if _, err := e.bridge.Prove(e.ctx, e.credentials.Token, proof); err != nil {
		t.Fatal(err)
	}
	changed := proof
	changed.CloseID = "another-close"
	changed.ProofID = "another-proof"
	if _, err := e.bridge.Prove(e.ctx, e.credentials.Token, changed); !errors.Is(err, sqlite.ErrConflict) {
		t.Fatalf("second exit replaced original: %v", err)
	}
	uses, err = e.store.Uses(e.ctx)
	if err != nil || !uses[0].Released {
		t.Fatalf("Use ACK preceded durable release: %+v %v", uses, err)
	}
}

func TestExecutionProofSettlesUnknownGrantOnlyInOriginalExecution(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "store.db"), true)
	first := e.start("first")
	second := e.start("second")
	_, lostGrant := e.acquire(first, "lost-ack")
	_, liveGrant := e.acquire(second, "live-ack")
	proof := executionProof(first)
	if _, err := e.bridge.Prove(e.ctx, e.credentials.Token, proof); err != nil {
		t.Fatal(err)
	}
	uses, err := e.store.Uses(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, use := range uses {
		if use.Approval.ID == lostGrant.ID && !use.Released {
			t.Fatal("unknown ACK Use leaked after original execution exit")
		}
		if use.Approval.ID == liveGrant.ID && use.Released {
			t.Fatal("another execution's Use was released")
		}
	}
	if err := e.bridge.ValidateUse(e.ctx, e.credentials.Token, liveGrant); err != nil {
		t.Fatalf("independent execution was fenced: %v", err)
	}
	if _, err := e.bridge.Prove(e.ctx, e.credentials.Token, proof); err != nil {
		t.Fatalf("lost proof ACK could not be repeated: %v", err)
	}
}

func TestExactBindingChildKeepsOriginalParentExecutionOwner(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "store.db"), true)
	parentPlan := e.start("parent")
	providerPlan := e.start("provider")
	_, parent := e.acquire(parentPlan, "parent-use")
	root := frame.ExportRef{InstallationID: "provider", Release: e.plugin.Revision(), UnitID: "view", Export: "preview", Contract: e.first.Ref()}
	binding, err := e.host.PlanWith(e.ctx, browser.RealmID(e.credentials.Realm), nil, root)
	if err != nil {
		t.Fatal(err)
	}
	child, err := e.bridge.AcquirePlan(e.ctx, e.credentials.Token, "child", binding, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if child.OperationID != "child" || child.ParentUseID != parent.ID || child.Execution != parentPlan.Execution || child.ScopeID != parentPlan.ScopeID || len(child.Objects) != 1 || child.Objects[0].Export.InstanceID != providerPlan.Execution.ID {
		t.Fatalf("child ownership was replaced by provider ownership: %+v", child)
	}
	if _, err := e.bridge.Prove(e.ctx, e.credentials.Token, useProof(parentPlan, child)); err != nil {
		t.Fatal(err)
	}
	if err := e.bridge.ValidateUse(e.ctx, e.credentials.Token, parent); err != nil {
		t.Fatalf("child close revoked parent: %v", err)
	}
	if _, err := e.bridge.Prove(e.ctx, e.credentials.Token, executionProof(parentPlan)); err != nil {
		t.Fatal(err)
	}
	attempts, err := e.store.Attempts(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range attempts {
		if record.Attempt.ID == providerPlan.Execution.ID && record.Released {
			t.Fatal("parent execution released independent provider execution")
		}
	}
}

func TestParentUseProofClosesUnknownChildGrantWithoutClosingExecution(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "store.db"), true)
	parentPlan := e.start("parent")
	providerPlan := e.start("provider")
	_, parent := e.acquire(parentPlan, "parent-use")
	root := frame.ExportRef{InstallationID: "provider", Release: e.plugin.Revision(), UnitID: "view", Export: "preview", Contract: e.first.Ref()}
	binding, err := e.host.PlanWith(e.ctx, browser.RealmID(e.credentials.Realm), nil, root)
	if err != nil {
		t.Fatal(err)
	}
	child, err := e.bridge.AcquirePlan(e.ctx, e.credentials.Token, "unknown-child", binding, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.bridge.Prove(e.ctx, e.credentials.Token, useProof(parentPlan, parent)); err != nil {
		t.Fatalf("parent actual exit could not settle unobserved child: %v", err)
	}
	uses, err := e.store.Uses(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, use := range uses {
		if (use.Approval.ID == child.ID || use.Approval.ID == parent.ID) && !use.Released {
			t.Fatalf("delegated child closure leaked: %+v", use)
		}
	}
	attempts, err := e.store.Attempts(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range attempts {
		if (attempt.Attempt.ID == parentPlan.Execution.ID || attempt.Attempt.ID == providerPlan.Execution.ID) && attempt.Released {
			t.Fatal("Use proof released an execution")
		}
	}
}

func TestPublicationWithdrawalPreservesAcceptedUse(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "store.db"), true)
	plan := e.start("view")
	request := browser.PublicationRequest{OperationID: "dynamic", Authority: plan.Authority, Realm: plan.Realm, Execution: plan.Execution, Object: browser.ObjectRef{ObjectID: "dynamic-object", Export: browser.LocalExport{InstanceID: plan.Units[0].Instance.ID, Name: "dynamic", Contract: e.first.Ref()}}, MetadataJSON: `{"label":"dynamic"}`}
	result, err := e.bridge.Publish(e.ctx, e.credentials.Token, request)
	if err != nil || result.Decision != browser.Accepted {
		t.Fatalf("publish: %+v %v", result, err)
	}
	_, grant := e.acquire(plan, "dynamic-use")
	if len(grant.Objects) != 3 {
		t.Fatalf("dynamic Point was not captured: %+v", grant.Objects)
	}
	withdrawn, err := e.bridge.Withdraw(e.ctx, e.credentials.Token, browser.Withdrawal{OperationID: "withdraw-dynamic", Authority: plan.Authority, PublicationID: result.Publication.ID})
	if err != nil || withdrawn.Decision != browser.Accepted {
		t.Fatalf("withdraw: %+v %v", withdrawn, err)
	}
	if err := e.bridge.ValidateUse(e.ctx, e.credentials.Token, grant); err != nil {
		t.Fatalf("withdrawal revoked an already admitted Use: %v", err)
	}
	snapshot, err := e.bridge.Snapshot(e.ctx, e.credentials.Token, []string{e.first.Ref().Name})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Points[0].Candidates) != 1 {
		t.Fatalf("withdrawn candidate remained visible: %+v", snapshot)
	}
	if err := e.bridge.CloseSnapshot(e.ctx, e.credentials.Token, snapshot.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.bridge.Prove(e.ctx, e.credentials.Token, useProof(plan, grant)); err != nil {
		t.Fatal(err)
	}
	proof := executionProof(plan)
	proof.ScopeKind = browser.PublicationScope
	proof.PublicationID = result.Publication.ID
	proof.CloseID = "publication-close"
	proof.ProofID = "publication-proof"
	if _, err := e.bridge.Prove(e.ctx, e.credentials.Token, proof); err != nil {
		t.Fatal(err)
	}
	attempts, err := e.store.Attempts(e.ctx)
	if err != nil || attempts[0].Released {
		t.Fatalf("subordinate proof released execution: %+v %v", attempts, err)
	}
}

func TestExecutionExitCoversAcceptedPublicationWithoutSeparateWithdrawal(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "store.db"), true)
	plan := e.start("view")
	request := browser.PublicationRequest{OperationID: "dynamic", Authority: plan.Authority, Realm: plan.Realm, Execution: plan.Execution, Object: browser.ObjectRef{ObjectID: "dynamic-object", Export: browser.LocalExport{InstanceID: plan.Units[0].Instance.ID, Name: "dynamic", Contract: e.first.Ref()}}, MetadataJSON: `{"label":"dynamic"}`}
	publication, err := e.bridge.Publish(e.ctx, e.credentials.Token, request)
	if err != nil || publication.Decision != browser.Accepted {
		t.Fatalf("publish: %+v %v", publication, err)
	}
	_, grant := e.acquire(plan, "dynamic-use")
	if len(grant.Objects) != 3 {
		t.Fatalf("dynamic object was not retained by its original Use: %+v", grant)
	}
	leaf := executionProof(plan)
	leaf.ScopeKind = browser.PublicationScope
	leaf.PublicationID = publication.Publication.ID
	leaf.CloseID = "publication-close"
	leaf.ProofID = "publication-proof"
	if _, err := e.bridge.Prove(e.ctx, e.credentials.Token, leaf); !errors.Is(err, frame.ErrStaleBinding) {
		t.Fatalf("an independent publication exit bypassed Host withdrawal: %v", err)
	}
	proof := executionProof(plan)
	for range 2 {
		if acknowledgement, err := e.bridge.Prove(e.ctx, e.credentials.Token, proof); err != nil || !acknowledgement.Recorded {
			t.Fatalf("original execution exit: %+v %v", acknowledgement, err)
		}
	}
	if err := e.bridge.ValidateUse(e.ctx, e.credentials.Token, grant); err == nil {
		t.Fatal("execution exit left its original delegated Use active")
	}
	attempts, err := e.store.Attempts(e.ctx)
	if err != nil || len(attempts) != 1 || !attempts[0].Released {
		t.Fatalf("execution exit was acknowledged before its attempt released: %+v %v", attempts, err)
	}
	uses, err := e.store.Uses(e.ctx)
	if err != nil || len(uses) != 1 || !uses[0].Released {
		t.Fatalf("execution exit was acknowledged before its Use released: %+v %v", uses, err)
	}
	snapshot, err := e.bridge.Snapshot(e.ctx, e.credentials.Token, []string{e.first.Ref().Name})
	if err != nil || len(snapshot.Points) != 1 || len(snapshot.Points[0].Candidates) != 0 {
		t.Fatalf("execution exit left its dynamic publication visible: %+v %v", snapshot, err)
	}
	if err := e.bridge.CloseSnapshot(e.ctx, e.credentials.Token, snapshot.ID); err != nil {
		t.Fatal(err)
	}
}

func TestControlHTTPAuthenticationAndStrictInput(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "store.db"), true)
	plan := e.start("http")
	call := func(token, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/frame/control", bytes.NewBufferString(body))
		request.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		e.bridge.Handler().ServeHTTP(recorder, request)
		return recorder
	}
	if response := call("invalid", `{"method":"poll","input":{}}`); response.Code != http.StatusUnauthorized {
		t.Fatalf("missing authentication: %d", response.Code)
	}
	if response := call(e.credentials.Token, `{"method":"poll","method":"acquire","input":{}}`); response.Code != http.StatusBadRequest {
		t.Fatalf("duplicate JSON key accepted: %d", response.Code)
	}
	if response := call(e.credentials.Token, `{"method":"poll","input":{"unknown":true}}`); response.Code != http.StatusBadRequest {
		t.Fatalf("unknown member accepted: %d", response.Code)
	}
	encoded, _ := json.Marshal(map[string]any{"method": "validatePlan", "input": plan})
	if response := call(e.credentials.Token, string(encoded)); response.Code != http.StatusOK || response.Body.String() != "null" {
		t.Fatalf("actual HTTP plan validation: %d %s", response.Code, response.Body.String())
	}
}

type recoveryFixture struct {
	Credentials browser.Credentials `json:"credentials"`
	Plan        browser.LocalPlan   `json:"plan"`
	Grant       browser.UseGrant    `json:"grant"`
}

func TestBridgeRecoveryHelper(t *testing.T) {
	path := os.Getenv("FRAME_BRIDGE_CRASH_DB")
	if path == "" {
		return
	}
	e := openBridge(t, path, false)
	plan := e.start("old")
	_, grant := e.acquire(plan, "old-unknown-ack")
	if err := json.NewEncoder(os.Stdout).Encode(recoveryFixture{Credentials: e.credentials, Plan: plan, Grant: grant}); err != nil {
		os.Exit(4)
	}
	os.Exit(0)
}

func TestHostReconstructionAcceptsOriginalProofWithoutRecreation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	command := exec.Command(os.Args[0], "-test.run=^TestBridgeRecoveryHelper$")
	command.Env = append(os.Environ(), "FRAME_BRIDGE_CRASH_DB="+path)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("helper: %v %s", err, output)
	}
	var fixture recoveryFixture
	if err := json.Unmarshal(output, &fixture); err != nil {
		t.Fatal(err)
	}
	e := openBridge(t, path, false)
	t.Cleanup(func() { _ = e.store.Close() })
	credentials, err := e.bridge.ResumeRealm(e.ctx, fixture.Credentials.Realm, fixture.Credentials.ResumeToken)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Authority.Epoch == fixture.Credentials.Authority.Epoch {
		t.Fatal("Host epoch was reused")
	}
	reopened, err := e.bridge.OpenRealm(e.ctx, "open-test", fixture.Credentials.ResumeToken)
	if err != nil || reopened.Realm != fixture.Credentials.Realm {
		t.Fatalf("original open operation created another Realm: %+v %v", reopened, err)
	}
	proof := executionProof(fixture.Plan)
	if _, err := e.bridge.Prove(e.ctx, credentials.Token, proof); err != nil {
		t.Fatal(err)
	}
	result, err := e.bridge.Reconnect(e.ctx, credentials.Token, browser.ReconnectRequest{Authority: fixture.Credentials.Authority, Realm: fixture.Credentials.Realm, Executions: []browser.Execution{}})
	if err != nil || result.Mode != browser.CleanupOnly {
		t.Fatalf("recovery: %+v %v", result, err)
	}
	dependencies, err := e.store.ExecutionDependencies(e.ctx)
	if err != nil || len(dependencies) != 0 {
		t.Fatalf("original delegated protection survived confirmed exit: %+v %v", dependencies, err)
	}
	if _, err := e.bridge.Acquire(e.ctx, credentials.Token, browser.AcquireRequest{OperationID: "new"}); !errors.Is(err, frame.ErrFenced) {
		t.Fatalf("old Realm admitted work: %v", err)
	}
	ack, err := e.bridge.Prove(e.ctx, credentials.Token, proof)
	if err != nil || !ack.Recorded || ack.Authority != credentials.Authority {
		t.Fatalf("repeated original proof: %+v %v", ack, err)
	}
}
