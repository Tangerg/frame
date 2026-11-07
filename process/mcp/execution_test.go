package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/process"
	"github.com/Tangerg/frame/process/runner"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

type fixtureSource struct{ program process.Program }

func (s fixtureSource) Program(context.Context, frame.UnitBinding) (process.Program, error) {
	return s.program, nil
}

// Exercise the installed declaration, approved binding, durable launch and final exit proof.
func checkExecution(t *testing.T, protocol process.Protocol, capability *portable.Capability, source func(*testing.T, string) process.Program, want error, check func(context.Context, process.Service, string)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "fm-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	realm, err := runner.Open(runner.Config{Directory: filepath.Join(dir, "runner")})
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "runner", "sock")
	listener, err := realm.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- realm.Serve(ctx, listener) }()
	defer func() {
		cancel()
		_ = listener.Close()
		<-stopped
		if err := realm.Close(); err != nil {
			t.Error(err)
		}
	}()
	admission := frame.NewAdmission()
	store, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(dir, "state.db"), Admission: admission})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	client, err := runner.NewClient(socket)
	if err != nil {
		t.Fatal(err)
	}
	cfg := process.Config{Runner: client, Store: store, Source: fixtureSource{source(t, dir)}, CloseTimeout: 3 * time.Second, MaxFrameBytes: 1 << 20, Protocol: protocol}
	driver, err := process.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	host, err := frame.NewHost(ctx, frame.HostConfig{Admission: admission, Epoch: store.Epoch(), Drivers: []frame.Driver{driver}, Installations: store, ExecutionProtection: store})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if outcome, err := host.Close(ctx); err != nil || !outcome.Released {
			t.Errorf("host close: %+v %v", outcome, err)
		}
	}()
	contract, err := frame.NewPortableContract[process.Service]("1", capability)
	if err != nil {
		t.Fatal(err)
	}
	ref := contract.Ref()
	if err := host.Register(contract); err != nil {
		t.Fatal(err)
	}
	unit := frame.UnitDeclaration{ID: "main", Profile: protocol.Profile(), Exports: []frame.ExportDeclaration{{Name: "service", Contract: ref}}}
	release, err := frame.Define(frame.Definition{Revision: value.Hash([]byte("process integration fixture")), Units: []frame.Unit{{Declaration: unit}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.Install(ctx, frame.InstallationConfig{ID: "installed", Plugin: release, Configuration: []byte("{}"), Enabled: true}); err != nil {
		t.Fatal(err)
	}
	export := frame.ExportRef{InstallationID: "installed", Release: release.Revision(), UnitID: "main", Export: "service", Contract: ref}
	plan, err := host.Plan(ctx, export)
	if err != nil {
		t.Fatal(err)
	}
	owner := frame.NewLifetime(ctx)
	scope, err := host.Admission().OpenScope(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	use, err := host.Acquire(ctx, plan, frame.UseConfig{Owner: owner, Scope: scope})
	if !errors.Is(err, want) {
		t.Fatalf("acquire = %v, want %v", err, want)
	}
	if err == nil {
		other := cfg
		other.Protocol = nil
		neutral, err := process.New(other)
		if err != nil {
			t.Fatal(err)
		}
		if err := neutral.Recover(ctx); err != nil {
			t.Fatalf("neutral recovery touched another profile's live attempt: %v", err)
		}
		if err := frame.BorrowExport[process.Service](ctx, use, export, func(ctx context.Context, svc process.Service) error {
			check(ctx, svc, dir)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := use.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := use.Close(ctx); err != nil {
			t.Fatalf("repeat close: %v", err)
		}
	}
	if _, err := owner.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if outcome, err := host.Close(ctx); err != nil || !outcome.Released {
		t.Fatalf("retire host: %+v %v", outcome, err)
	}
	records, err := store.Attempts(ctx)
	if err != nil || len(records) != 1 || records[0].Attempt.Profile != protocol.Profile() || !records[0].ExitProven() || records[0].Proof == nil {
		t.Fatalf("original attempt: %+v %v", records, err)
	}
	exit, err := client.Inspect(ctx, records[0].Attempt.ID)
	if err != nil || exit.State != runner.Exited || exit.ProofID != records[0].Proof.ExitID {
		t.Fatalf("actual process exit: %+v %v", exit, err)
	}
}
