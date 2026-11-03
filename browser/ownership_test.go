package browser_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
	"github.com/Tangerg/frame/sqlite"
)

func TestReturnedGrantCannotChangeAdmission(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true)
	p := e.start("component")
	req, grant := e.acquire(p, "original")
	grant.Objects[0].ObjectID = "changed"
	if err := e.bridge.ValidateUse(e.ctx, e.credentials.Token, grant); !errors.Is(err, frame.ErrStaleBinding) {
		t.Fatalf("changed grant: %v", err)
	}
	replay, err := e.bridge.Acquire(e.ctx, e.credentials.Token, req)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.bridge.ValidateUse(e.ctx, e.credentials.Token, replay); err != nil {
		t.Fatal(err)
	}
	if err = e.host.RetireInstance(e.ctx, p.Execution.ID); err != nil {
		t.Fatal(err)
	}
	if result, err := e.bridge.AdmitPlan(e.ctx, e.credentials.Token, p); err != nil || result.Decision != browser.PlanCleanupRequired {
		t.Fatalf("retiring instance lost its original cleanup obligation: %+v %v", result, err)
	}
}

func TestSealedEmptyRealmReturnsLiveCapacity(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true)
	b, err := browser.NewBridge(browser.BridgeConfig{Host: e.host, Store: e.store, Source: testModules{}, MaxRealms: 1})
	if err != nil {
		t.Fatal(err)
	}
	first, err := b.OpenRealm(e.ctx, "first", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if err = b.SealRealm(e.ctx, first.Token); err != nil {
		t.Fatal(err)
	}
	if _, err = b.OpenRealm(e.ctx, "second", "1123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Snapshot(e.ctx, first.Token, snapshotRequest(e.first.Ref().Name)); !errors.Is(err, frame.ErrFenced) {
		t.Fatal(err)
	}
	if _, err = b.Poll(e.ctx, first.Token); err != nil {
		t.Fatal(err)
	}
}

func TestRealmApplicationOutlivesItsOpeningRequest(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true)
	opening, cancel := context.WithCancel(e.ctx)
	credentials, err := e.bridge.OpenRealm(opening, "open-application", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	e.credentials = credentials
	plan := e.start("application")
	_, grant := e.acquire(plan, "application-use")
	if err := e.bridge.ValidateUse(e.ctx, credentials.Token, grant); err != nil {
		t.Fatalf("request cancellation closed the Realm application: %v", err)
	}
}

func TestApplicationUseRetainsItsRetiringProviderUntilExecutionProof(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true)
	plan := e.start("provider")
	_, grant := e.acquire(plan, "application-use")
	if err := e.host.RetireInstance(e.ctx, plan.Execution.ID); err != nil {
		t.Fatal(err)
	}
	retained, err := e.store.Use(e.ctx, grant.ID)
	if err != nil || retained.Released {
		t.Fatalf("provider retirement substituted for the application's exit: %+v %v", retained, err)
	}
	if ack, err := e.bridge.Prove(e.ctx, e.credentials.Token, executionProof(plan)); err != nil || !ack.Recorded {
		t.Fatalf("original execution proof did not close its application Uses: %+v %v", ack, err)
	}
	retained, err = e.store.Use(e.ctx, grant.ID)
	if err != nil || !retained.Released {
		t.Fatalf("proven execution retained its application Use: %+v %v", retained, err)
	}
}

type foreignSource struct{ called bool }

func (s *foreignSource) Module(context.Context, frame.UnitBinding) (browser.Artifact, error) {
	s.called = true
	return browser.Artifact{}, frame.ErrInvalid
}

func TestPrepareRefusesForeignHostBeforeSource(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "original.sqlite"), true)
	source := new(foreignSource)
	b, err := browser.NewBridge(browser.BridgeConfig{Host: e.host, Store: e.store, Source: source})
	if err != nil {
		t.Fatal(err)
	}
	cred, err := b.OpenRealm(e.ctx, "original", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	gate := frame.NewAdmission()
	db, err := sqlite.Open(e.ctx, sqlite.Config{Path: filepath.Join(t.TempDir(), "foreign.sqlite"), Admission: gate})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h, err := frame.NewHost(e.ctx, frame.HostConfig{Admission: gate, Epoch: db.Epoch(), Installations: db, ExecutionProtection: db})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close(e.ctx)
	if err = h.Register(e.first, e.second); err != nil {
		t.Fatal(err)
	}
	if err = h.RegisterDriver(b); err != nil {
		t.Fatal(err)
	}
	if _, err = h.Install(e.ctx, frame.InstallationConfig{ID: "foreign", Plugin: e.plugin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	_, err = h.ActivateUnit(e.ctx, browser.RealmID(cred.Realm), frame.UnitRef{InstallationID: "foreign", Release: e.plugin.Revision(), UnitID: "view"})
	if !errors.Is(err, frame.ErrFenced) || source.called {
		t.Fatalf("foreign setup reached source=%v: %v", source.called, err)
	}
}

func TestAcquisitionRefusalDoesNotConsumePendingBudget(t *testing.T) {
	e, b := snapshotBridge(t)
	r, err := b.OpenRealm(e.ctx, "one", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.host.Install(e.ctx, frame.InstallationConfig{ID: "component", Plugin: e.plugin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	root := frame.ExportRef{InstallationID: "component", Release: e.plugin.Revision(), UnitID: "view", Export: "preview", Contract: e.first.Ref()}
	plan, err := e.host.PlanWith(e.ctx, browser.RealmID(r.Realm), nil, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"first", "second", "third", "first"} {
		_, err = b.AcquirePlan(e.ctx, r.Token, op, plan, "missing")
		if !errors.Is(err, frame.ErrStaleBinding) {
			t.Fatalf("%s: %v", op, err)
		}
	}
}

func TestUnadmittedRetirementRetriesOriginalProof(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	e := openBridge(t, path, true)
	cred, err := e.bridge.OpenRealm(e.ctx, "op", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.host.Install(e.ctx, frame.InstallationConfig{ID: "component", Plugin: e.plugin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.host.ActivateUnit(e.ctx, browser.RealmID(cred.Realm), frame.UnitRef{InstallationID: "component", Release: e.plugin.Revision(), UnitID: "view"})
		done <- err
	}()
	var id string
	for id == "" {
		records, err := e.store.Attempts(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(records) > 0 {
			id = records[0].Attempt.ID
			break
		}
		select {
		case <-e.ctx.Done():
			t.Fatal(e.ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("CREATE TRIGGER refuse_exit BEFORE UPDATE OF state ON frame_attempts BEGIN SELECT RAISE(ABORT,'injected'); END"); err != nil {
		t.Fatal(err)
	}
	if err = e.host.RetireInstance(e.ctx, id); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err == nil {
		t.Fatal("cancelled setup succeeded")
	}
	poll, err := e.bridge.Poll(e.ctx, cred.Token)
	if err != nil || len(poll.Plans) != 0 {
		t.Fatalf("retired plan exposed: %+v, %v", poll, err)
	}
	if err = e.bridge.SealRealm(e.ctx, cred.Token); err == nil {
		t.Fatal("failed durable release acknowledged")
	}
	if _, err = db.Exec("DROP TRIGGER refuse_exit"); err != nil {
		t.Fatal(err)
	}
	if err = e.bridge.SealRealm(e.ctx, cred.Token); err != nil {
		t.Fatal(err)
	}
	record, err := e.store.Attempt(e.ctx, id)
	if err != nil || !record.Released || record.Proof.Kind != "not-admitted" {
		t.Fatalf("release: %+v, %v", record, err)
	}
	if err = e.host.WaitInstanceRetired(e.ctx, id); err != nil {
		t.Fatal(err)
	}
}

type blockingModules struct{ entered, resume chan struct{} }

func (s blockingModules) Module(ctx context.Context, _ frame.UnitBinding) (browser.Artifact, error) {
	select {
	case s.entered <- struct{}{}:
	case <-ctx.Done():
		return browser.Artifact{}, context.Cause(ctx)
	}
	select {
	case <-s.resume:
		return browser.Artifact{}, frame.ErrUnavailable
	case <-ctx.Done():
		return browser.Artifact{}, context.Cause(ctx)
	}
}

func TestPrepareReservesCapacityBeforeResolvingAModule(t *testing.T) {
	source := blockingModules{make(chan struct{}, 2), make(chan struct{})}
	e := openConfiguredBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true, func(c *browser.BridgeConfig) { c.Source = source; c.MaxPending = 1 })
	cred, err := e.bridge.OpenRealm(e.ctx, "original", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []frame.InstallationID{"first", "second"} {
		if _, err = e.host.Install(e.ctx, frame.InstallationConfig{ID: id, Plugin: e.plugin, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	activate := func(id frame.InstallationID) error {
		_, err := e.host.ActivateUnit(e.ctx, browser.RealmID(cred.Realm), frame.UnitRef{InstallationID: id, Release: e.plugin.Revision(), UnitID: "view"})
		return err
	}
	done := make(chan error, 1)
	go func() { done <- activate("first") }()
	select {
	case <-source.entered:
	case <-e.ctx.Done():
		t.Fatal(e.ctx.Err())
	}
	second := make(chan error, 1)
	go func() { second <- activate("second") }()
	select {
	case <-source.entered:
		close(source.resume)
		t.Fatal("second creation passed the only pending slot")
	case err := <-second:
		if !errors.Is(err, frame.ErrCapacity) {
			close(source.resume)
			t.Fatal(err)
		}
	case <-e.ctx.Done():
		close(source.resume)
		t.Fatal(e.ctx.Err())
	}
	close(source.resume)
	if err = <-done; !errors.Is(err, frame.ErrUnavailable) {
		t.Fatal(err)
	}
}

func TestRetiredExecutionCannotProveAnotherPublication(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true)
	old := e.start("retired")
	live := e.start("live")
	if _, err := e.bridge.Prove(e.ctx, e.credentials.Token, executionProof(old)); err != nil {
		t.Fatal(err)
	}
	request := browser.PublicationRequest{OperationID: "publish", Authority: live.Authority, Realm: live.Realm, Execution: live.Execution, Object: browser.ObjectRef{ObjectID: "new-object", Export: browser.LocalExport{InstanceID: live.Execution.ID, Name: "dynamic", Contract: e.first.Ref()}}, MetadataJSON: `{"label":"dynamic"}`}
	published, err := e.bridge.Publish(e.ctx, e.credentials.Token, request)
	if err != nil || published.Decision != browser.Accepted {
		t.Fatalf("publish: %+v, %v", published, err)
	}
	foreign := executionProof(old)
	foreign.ScopeKind = browser.PublicationScope
	foreign.PublicationID = published.Publication.ID
	foreign.ProofID = "forged-publication"
	foreign.CloseID = "forged-close"
	if _, err = e.bridge.Prove(e.ctx, e.credentials.Token, foreign); !errors.Is(err, frame.ErrStaleBinding) {
		t.Fatalf("foreign publication proof: %v", err)
	}
	withdrawn, err := e.bridge.Withdraw(e.ctx, e.credentials.Token, browser.Withdrawal{Authority: live.Authority, PublicationID: published.Publication.ID})
	if err != nil || withdrawn.Decision != browser.Accepted {
		t.Fatalf("original publication lost: %+v, %v", withdrawn, err)
	}
	proof := executionProof(live)
	proof.ScopeKind = browser.PublicationScope
	proof.PublicationID = published.Publication.ID
	proof.ProofID = "real-publication"
	proof.CloseID = "real-close"
	if _, err = e.bridge.Prove(e.ctx, e.credentials.Token, proof); err != nil {
		t.Fatal(err)
	}
}

func TestUnicodeAcquisitionReceiptDoesNotConsumeCapacity(t *testing.T) {
	e := openConfiguredBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true, func(c *browser.BridgeConfig) { c.MaxPending = 1 })
	plan := e.start("test")
	root := frame.ExportRef{InstallationID: "test", Release: e.plugin.Revision(), UnitID: "view", Export: "preview", Contract: e.first.Ref()}
	binding, err := e.host.PlanWith(e.ctx, browser.RealmID(e.credentials.Realm), nil, root)
	if err != nil {
		t.Fatal(err)
	}
	var original browser.UseGrant
	for _, id := range []string{"操作一", "操作二", "操作一"} {
		grant, err := e.bridge.AcquirePlan(e.ctx, e.credentials.Token, id, binding, "")
		if err != nil {
			t.Fatal(err)
		}
		if original.ID == "" {
			original = grant
		}
		if id == "操作一" && grant.ID != original.ID {
			t.Fatal("Unicode replay minted another grant")
		}
		if _, err = e.bridge.Prove(e.ctx, e.credentials.Token, useProof(plan, grant)); err != nil {
			t.Fatal(err)
		}
	}
}

type databaseWait struct {
	context.Context
	once    sync.Once
	waiting chan struct{}
}

func (c *databaseWait) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestReceiptLookupsShareAndRespectPendingCapacity(t *testing.T) {
	e := openConfiguredBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true, func(c *browser.BridgeConfig) { c.MaxPending = 1 })
	cred, err := e.bridge.OpenRealm(e.ctx, "readers", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	locked, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	readDone := make(chan error, 1)
	go func() {
		readDone <- e.store.Read(e.ctx, func(sqlite.Reader) error { close(locked); <-release; return nil })
	}()
	<-locked
	start := func(id string) (<-chan error, <-chan struct{}) {
		ctx := &databaseWait{Context: e.ctx, waiting: make(chan struct{})}
		result := make(chan error, 1)
		go func() { _, err := e.bridge.Publication(ctx, cred.Token, id); result <- err }()
		return result, ctx.waiting
	}
	first, waiting := start("original")
	select {
	case <-waiting:
	case <-e.ctx.Done():
		t.Fatal(e.ctx.Err())
	}
	second, waiting := start("original")
	select {
	case <-waiting:
	case <-e.ctx.Done():
		t.Fatal(e.ctx.Err())
	}
	if _, err = e.bridge.Publication(e.ctx, cred.Token, "different"); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("other lookup: %v", err)
	}
	unblock()
	for _, done := range []<-chan error{readDone, first, second} {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestBrowserOwnsCleanupBeforeRecordingCreation(t *testing.T) {
	e := openConfiguredBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true, func(c *browser.BridgeConfig) {
		c.PublicConfiguration = func(_ context.Context, s *frame.Setup) ([]byte, error) {
			for {
				_, err := s.Cleanup("test resource", func() error { return nil })
				if errors.Is(err, frame.ErrCapacity) {
					return nil, nil
				}
				if err != nil {
					return nil, err
				}
			}
		}
	})
	cred, err := e.bridge.OpenRealm(e.ctx, "full", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.host.Install(e.ctx, frame.InstallationConfig{ID: "component", Plugin: e.plugin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	_, err = e.host.ActivateUnit(e.ctx, browser.RealmID(cred.Realm), frame.UnitRef{InstallationID: "component", Release: e.plugin.Revision(), UnitID: "view"})
	if !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("full lifetime: %v", err)
	}
	records, err := e.store.Attempts(e.ctx)
	if err != nil || len(records) != 0 {
		t.Fatalf("creation approved without reserved cleanup: %v, %v", records, err)
	}
	if err := e.host.WaitRetired(e.ctx, "component"); err != nil {
		t.Fatal(err)
	}
}
