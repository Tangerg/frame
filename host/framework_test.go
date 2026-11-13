package host_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

func TestScopedStartSharesOnlyTheOriginalGrantAndFinalStop(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	var starts, stops atomic.Int32
	unit, err := framehost.DefineUnit(framehost.UnitSpec{ID: "watcher", Prepare: func(s *framehost.Setup) error {
		if s.ScopeGrant().ID() != "tenant-a" {
			return errors.New("unexpected execution scope")
		}
		_, err := s.Cleanup("index", func() error { stops.Add(1); return nil })
		if err != nil {
			return err
		}
		return s.OnActivate(func(context.Context) error { starts.Add(1); return nil })
	}})
	if err != nil {
		t.Fatal(err)
	}
	plugin, err := framehost.Define(framehost.Definition{Revision: value.Hash([]byte("one")), Units: []framehost.Unit{unit}})
	if err != nil {
		t.Fatal(err)
	}
	id := framehost.InstallationID(strings.Repeat("x", frame.MaxNameBytes))
	installed, err := host.Install(ctx, framehost.InstallationConfig{ID: id, Plugin: plugin, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	host.Admission().FenceScope(framehost.ScopeID(id))
	a, err := host.Admission().OpenScope(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := host.Admission().OpenScope(ctx, "tenant-b")
	if err != nil {
		t.Fatal(err)
	}
	root := installed.Unit("watcher")
	first, err := host.StartUnit(ctx, a, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.StartUnit(ctx, b, root); !errors.Is(err, framehost.ErrFenced) {
		t.Fatalf("another scope reused the original runtime: %v", err)
	}
	second, err := host.StartUnit(ctx, a, root)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID() != second.ID() || starts.Load() != 1 {
		t.Fatal("duplicate start")
	}
	if err := first.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if stops.Load() != 0 {
		t.Fatal("closing the first handle stopped its sibling")
	}
	if err := second.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := second.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if stops.Load() != 1 {
		t.Fatalf("stops=%d", stops.Load())
	}
	third, err := host.StartUnit(ctx, a, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.SetEnabled(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if err := host.WaitInstanceRetired(ctx, third.ID()); err != nil {
		t.Fatalf("unclosed start handle blocked disable: %v", err)
	}
	if err := third.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRejectedUseNeverPreparesCode(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	var prepares atomic.Int32
	p := installNative(t, host, "watcher", framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo}, Setup: func(context.Context, *framehost.Setup) ([]framehost.ExportValue, error) {
		prepares.Add(1)
		return nil, nil
	}})
	root := framehost.UnitRef{InstallationID: "watcher", Release: p.Revision(), UnitID: "main"}
	plan, err := host.PlanUnit(ctx, "", root, nil)
	if err != nil {
		t.Fatal(err)
	}

	owner, grant := ownerAndScope(t, host)
	for _, cfg := range []framehost.UseConfig{{Owner: owner}, {Scope: grant}} {
		if _, err := host.Acquire(ctx, plan, cfg); err == nil {
			t.Fatal("plan admitted")
		}
		if _, err := host.AcquireRecord(ctx, "unknown-task", cfg); err == nil {
			t.Fatal("restore admitted")
		}
	}
	good, err := host.Acquire(ctx, plan, framehost.UseConfig{Owner: owner, Scope: grant})
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, time.Millisecond)
	if _, err := good.WaitRetired(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	cancel()
	if err := host.ValidateUse(ctx, good); err != nil {
		t.Fatalf("waiting closed the live use: %v", err)
	}
	if _, err := host.Acquire(ctx, plan, framehost.UseConfig{Owner: owner, Parent: good, Scope: grant}); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("child escaped lifetime: %v", err)
	}
	if _, err := good.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if prepares.Load() != 1 {
		t.Fatalf("rejected preparation executed: %d", prepares.Load())
	}
}

func TestRevocationDuringPrepareRefusesActivation(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	preparing := make(chan struct{})
	var activated atomic.Bool
	p := installNative(t, host, "slow", framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo}, Setup: func(ctx context.Context, s *framehost.Setup) ([]framehost.ExportValue, error) {
		if err := s.OnActivate(func(context.Context) error { activated.Store(true); return nil }); err != nil {
			return nil, err
		}
		close(preparing)
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}})
	owner, grant := ownerAndScope(t, host)
	done := make(chan error, 1)
	go func() {
		_, err := host.AcquireUnit(ctx, framehost.UnitRef{InstallationID: "slow", Release: p.Revision(), UnitID: "main"}, framehost.UseConfig{Owner: owner, Scope: grant})
		done <- err
	}()
	select {
	case <-preparing:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	host.Admission().FenceScope(grant.ID())
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("revoked activation published")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if activated.Load() {
		t.Fatal("revocation ran OnActivate")
	}
}

func TestOptionalAbsenceRequiresTheDeclaredContract(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	files, _ := framehost.NewContract[string]("files", "1")
	count, _ := framehost.NewContract[int]("count", "1")
	if err := host.Register(files, count); err != nil {
		t.Fatal(err)
	}
	absent := framehost.Optional("source", files)
	foreign := framehost.Optional("source", count)
	u, err := framehost.DefineUnit(framehost.UnitSpec{ID: "main", Imports: framehost.Imports(absent), Prepare: func(s *framehost.Setup) error {
		if _, ok, err := absent.Get(s); ok || err != nil {
			return errors.New("valid absence was rejected")
		}
		if _, ok, err := foreign.Get(s); ok || !errors.Is(err, frame.ErrContractMismatch) {
			return errors.New("wrong contract admitted")
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	p := installNative(t, host, "optional", u)
	if _, err := host.ActivateUnit(ctx, "", framehost.UnitRef{InstallationID: "optional", Release: p.Revision(), UnitID: "main"}); err != nil {
		t.Fatal(err)
	}
}
