//go:build frame_interop

package conformance_test

import (
	"context"
	_ "embed"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/integrity"
	"github.com/Tangerg/frame/jsonvalue"
	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/process"
	"github.com/Tangerg/frame/runner"
	"github.com/Tangerg/frame/sqlite"
)

//go:embed duplex/service.json
var descriptor []byte

type upstream struct {
	started   chan struct{}
	stopped   chan struct{}
	waitCalls atomic.Int32
}

func (u *upstream) Call(ctx context.Context, member string, input jsonvalue.Value) (jsonvalue.Value, error) {
	switch member {
	case "probe":
		return input, nil
	case "wait":
		if u.waitCalls.Add(1) != 1 {
			return jsonvalue.Value{}, errors.New("duplicate wait")
		}
		close(u.started)
		<-ctx.Done()
		close(u.stopped)
		return jsonvalue.Value{}, context.Cause(ctx)
	default:
		return jsonvalue.Value{}, frame.ErrContractMismatch
	}
}
func (u *upstream) Stream(ctx context.Context, member string, input jsonvalue.Value, emit process.Emit) (jsonvalue.Value, error) {
	if member != "read" {
		return jsonvalue.Value{}, frame.ErrContractMismatch
	}
	for _, value := range []string{"first", "second"} {
		chunk, err := jsonvalue.Encode(value)
		if err != nil {
			return jsonvalue.Value{}, err
		}
		if err := emit(ctx, chunk); err != nil {
			return jsonvalue.Value{}, err
		}
	}
	return input, nil
}

type program struct{ p process.Program }

func (p program) Program(context.Context, frame.UnitBinding) (process.Program, error) {
	return p.p, nil
}

func install(ctx context.Context, t *testing.T, h *frame.Host, id frame.InstallationID, unit frame.Unit) frame.UnitRef {
	t.Helper()
	release, err := frame.Define(frame.Definition{Revision: integrity.Hash([]byte(id)), Units: []frame.Unit{unit}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Install(ctx, frame.InstallationConfig{ID: id, Plugin: release, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	return frame.UnitRef{InstallationID: id, Release: release.Revision(), UnitID: unit.Declaration.ID}
}

func TestNodeRelaysStreamingAndCancelsOriginalGoCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	directory, err := os.MkdirTemp("", "frame-duplex-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	server, err := runner.Open(runner.Config{Directory: filepath.Join(directory, "runner")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
	}()
	socket := filepath.Join(directory, "runner", "socket")
	listener, err := server.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	control, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(control, listener) }()
	defer func() { stop(); _ = listener.Close(); <-done }()
	gate := frame.NewAdmission()
	store, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(directory, "db"), Admission: gate})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	node, err = filepath.EvalSymlinks(node)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(node)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := filepath.Abs("duplex/worker.mjs")
	if err != nil {
		t.Fatal(err)
	}
	driver, err := process.New(process.Config{Runner: runner.Dial(socket), Store: store, MaxFrameBytes: 1 << 20, CloseTimeout: 5 * time.Second, Source: program{process.Program{Command: node, ExecutableDigest: integrity.Hash(data), Args: []string{entry}, Directory: filepath.Dir(entry)}}})
	if err != nil {
		t.Fatal(err)
	}
	h, err := frame.NewHost(ctx, frame.HostConfig{Admission: gate, Epoch: store.Epoch(), Installations: store, ExecutionProtection: store, Drivers: []frame.Driver{driver}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		c, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_, err := h.Close(c)
		if err != nil {
			t.Error(err)
		}
	}()
	c, err := portable.ParseCapability(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	ref := frame.ContractRef{Name: c.Descriptor().Reference, Version: "1", Kind: frame.ServiceKind, Mode: frame.PortableMode, Digest: c.Identity().Digest()}
	contract, err := frame.BindContract[process.Service](ref, c.Identity().JSON())
	if err != nil {
		t.Fatal(err)
	}
	if err = h.Register(contract); err != nil {
		t.Fatal(err)
	}
	original := &upstream{started: make(chan struct{}), stopped: make(chan struct{})}
	source := install(ctx, t, h, "source", frame.Unit{Declaration: frame.UnitDeclaration{ID: "provider", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "upstream", Contract: ref}}}, Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
		return []frame.ExportValue{frame.Provide("upstream", contract, process.Service(original))}, nil
	}})
	provider := frame.ExportRef{InstallationID: source.InstallationID, UnitID: source.UnitID, Release: source.Release, Export: "upstream", Contract: ref}
	worker := install(ctx, t, h, "node", frame.Unit{Declaration: frame.UnitDeclaration{ID: "worker", Profile: frame.LocalProcess, Imports: []frame.ImportDeclaration{{Slot: "upstream", Contract: ref, Providers: []frame.ExportRef{provider}}}, Exports: []frame.ExportDeclaration{{Name: "relay", Contract: ref}}}})
	export := frame.ExportRef{InstallationID: worker.InstallationID, UnitID: worker.UnitID, Release: worker.Release, Export: "relay", Contract: ref}
	plan, err := h.Plan(ctx, export)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := gate.OpenScope(ctx, "document")
	if err != nil {
		t.Fatal(err)
	}
	err = frame.WithPlan(ctx, h, plan, grant, func(use *frame.Use) error {
		return frame.BorrowExport[process.Service](ctx, use, export, func(ctx context.Context, relay process.Service) error {
			input, _ := jsonvalue.Encode("done")
			var seen []string
			final, err := relay.(process.StreamingService).Stream(ctx, "read", input, func(_ context.Context, chunk jsonvalue.Value) error {
				seen = append(seen, chunk.String())
				return nil
			})
			if err != nil {
				return err
			}
			if final != input || !slices.Equal(seen, []string{`"first"`, `"second"`}) {
				return errors.New("nested stream altered chunk order or final value")
			}
			child, cancel := context.WithCancel(ctx)
			defer cancel()
			response := make(chan error, 1)
			go func() { _, err := relay.Call(child, "wait", input); response <- err }()
			select {
			case <-original.started:
			case <-ctx.Done():
				return context.Cause(ctx)
			}
			cancel()
			select {
			case err := <-response:
				if !errors.Is(err, process.ErrOutcomeUnknown) || !errors.Is(err, context.Canceled) {
					return errors.New("cancelled invocation lost original uncertain outcome")
				}
			case <-ctx.Done():
				return context.Cause(ctx)
			}
			select {
			case <-original.stopped:
			case <-ctx.Done():
				return context.Cause(ctx)
			}
			value, err := relay.Call(ctx, "probe", input)
			if err != nil {
				return err
			}
			if value != input || original.waitCalls.Load() != 1 {
				return errors.New("cancellation replayed work or disconnected an independent request")
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = h.SetEnabled(ctx, "node", false); err != nil {
		t.Fatal(err)
	}
	if err = h.WaitRetired(ctx, "node"); err != nil {
		t.Fatal(err)
	}
	attempts, err := store.Attempts(ctx)
	if err != nil || len(attempts) != 1 || !attempts[0].Released || attempts[0].PendingHandoff {
		t.Fatalf("unsettled original execution: %v %v", attempts, err)
	}
}
