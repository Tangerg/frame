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
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/process"
	"github.com/Tangerg/frame/process/runner"
	"github.com/Tangerg/frame/sqlite"
	framevalue "github.com/Tangerg/frame/value"
)

//go:embed duplex/service.json
var descriptor []byte

type upstream struct {
	started      chan struct{}
	stopped      chan struct{}
	waitCalls    atomic.Int32
	commandCalls atomic.Int32
}

func (u *upstream) Invoke(ctx context.Context, invocation frame.Invocation) (framevalue.JSON, error) {
	switch invocation.Member() {
	case "probe":
		return invocation.Input(), nil
	case "replace":
		if invocation.CommandID() != "original-command" || invocation.Kind() != frame.Command {
			return framevalue.JSON{}, errors.New("relay lost original command identity")
		}
		u.commandCalls.Add(1)
		return invocation.Input(), nil
	case "wait":
		if u.waitCalls.Add(1) != 1 {
			return framevalue.JSON{}, errors.New("duplicate wait")
		}
		close(u.started)
		<-ctx.Done()
		close(u.stopped)
		return framevalue.JSON{}, context.Cause(ctx)
	default:
		return framevalue.JSON{}, frame.ErrContractMismatch
	}
}
func (u *upstream) Stream(ctx context.Context, invocation frame.Invocation, emit frame.Emit) (framevalue.JSON, error) {
	if invocation.Member() != "read" {
		return framevalue.JSON{}, frame.ErrContractMismatch
	}
	for _, value := range []string{"first", "second"} {
		chunk, err := framevalue.EncodeJSON(value)
		if err != nil {
			return framevalue.JSON{}, err
		}
		if err := emit(ctx, chunk); err != nil {
			return framevalue.JSON{}, err
		}
	}
	return invocation.Input(), nil
}

type program struct{ p process.Program }

func (p program) Program(context.Context, framehost.UnitBinding) (process.Program, error) {
	return p.p, nil
}

func install(ctx context.Context, t *testing.T, h framehost.Host, id framehost.InstallationID, unit framehost.Unit) framehost.UnitRef {
	t.Helper()
	release, err := framehost.Define(framehost.Definition{Revision: framevalue.Hash([]byte(id)), Units: []framehost.Unit{unit}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Install(ctx, framehost.InstallationConfig{ID: id, Plugin: release, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	return framehost.UnitRef{InstallationID: id, Release: release.Revision(), UnitID: unit.Declaration.ID}
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
	gate := framehost.NewAdmission()
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
	client, err := runner.NewClient(socket)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := process.New(process.Config{Runner: client, Store: store, MaxFrameBytes: 1 << 20, CloseTimeout: 5 * time.Second, Source: program{process.Program{Command: node, ExecutableDigest: framevalue.Hash(data), Args: []string{entry}, Directory: filepath.Dir(entry)}}})
	if err != nil {
		t.Fatal(err)
	}
	h, err := framehost.New(ctx, framehost.Config{Admission: gate, Epoch: store.Epoch(), Installations: store, ExecutionProtection: store, Drivers: []framehost.Driver{driver}})
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
	c, err := frame.ParseCapability(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := framehost.NewPortableContract[frame.Service]("1", c)
	if err != nil {
		t.Fatal(err)
	}
	ref := contract.Ref()
	if err = h.Register(contract); err != nil {
		t.Fatal(err)
	}
	original := &upstream{started: make(chan struct{}), stopped: make(chan struct{})}
	source := install(ctx, t, h, "source", framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "provider", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "upstream", Contract: ref}}}, Setup: func(context.Context, *framehost.Setup) ([]framehost.ExportValue, error) {
		return []framehost.ExportValue{framehost.Provide("upstream", contract, frame.Service(original))}, nil
	}})
	provider := framehost.ExportRef{InstallationID: source.InstallationID, UnitID: source.UnitID, Release: source.Release, Export: "upstream", Contract: ref}
	worker := install(ctx, t, h, "node", framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "worker", Profile: framehost.LocalProcess, Imports: []framehost.ImportDeclaration{{Slot: "upstream", Contract: ref, Providers: []framehost.ExportRef{provider}}}, Exports: []framehost.ExportDeclaration{{Name: "relay", Contract: ref}}}})
	export := framehost.ExportRef{InstallationID: worker.InstallationID, UnitID: worker.UnitID, Release: worker.Release, Export: "relay", Contract: ref}
	plan, err := h.Plan(ctx, export)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := gate.OpenScope(ctx, "document")
	if err != nil {
		t.Fatal(err)
	}
	err = framehost.WithPlan(ctx, h, plan, grant, func(use *framehost.Use) error {
		return framehost.BorrowExport[frame.Service](ctx, use, export, func(ctx context.Context, relay frame.Service) error {
			input, _ := framevalue.EncodeJSON("done")
			var seen []string
			final, err := relay.(frame.StreamingService).Stream(ctx, queryInvocation(t, "read", input), func(_ context.Context, chunk framevalue.JSON) error {
				seen = append(seen, chunk.String())
				return nil
			})
			if err != nil {
				return err
			}
			if final != input || !slices.Equal(seen, []string{`"first"`, `"second"`}) {
				return errors.New("nested stream altered chunk order or final value")
			}
			command, err := frame.NewCommandInvocation("original-command", "replace", input)
			if err != nil {
				return err
			}
			result, err := relay.Invoke(ctx, command)
			if err != nil || result != input || original.commandCalls.Load() != 1 {
				return errors.Join(errors.New("command relay changed its identity or outcome"), err)
			}
			child, cancel := context.WithCancel(ctx)
			defer cancel()
			response := make(chan error, 1)
			go func() { _, err := relay.Invoke(child, queryInvocation(t, "wait", input)); response <- err }()
			select {
			case <-original.started:
			case <-ctx.Done():
				return context.Cause(ctx)
			}
			cancel()
			select {
			case err := <-response:
				if !errors.Is(err, frame.ErrOutcomeUnknown) || !errors.Is(err, context.Canceled) {
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
			value, err := relay.Invoke(ctx, queryInvocation(t, "probe", input))
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
	if err != nil || len(attempts) != 1 || attempts[0].State != sqlite.AttemptReleased {
		t.Fatalf("unsettled original execution: %v %v", attempts, err)
	}
}

func queryInvocation(t *testing.T, member string, input framevalue.JSON) frame.Invocation {
	t.Helper()
	invocation, err := frame.NewQueryInvocation(member, input)
	if err != nil {
		t.Fatal(err)
	}
	return invocation
}
