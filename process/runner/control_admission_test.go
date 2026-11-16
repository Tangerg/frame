package runner

import (
	"bufio"
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/Tangerg/frame"
)

func TestInvalidExitReceiptIsRejectedBeforeConnecting(t *testing.T) {
	client, daemon := unavailableControls(t)
	ref := AttemptRef{AttemptID: "attempt", Epoch: 1, RunnerID: "runner"}
	for _, state := range []State{Starting, Running, Unknown} {
		receipt := ExitReceipt{AttemptRef: ref, State: state, ProofID: "proof"}
		for _, boundary := range []struct {
			name        string
			acknowledge func(context.Context, ExitReceipt) error
		}{
			{"client", client.AcknowledgeExit},
			{"daemon", daemon.AcknowledgeExit},
		} {
			t.Run(boundary.name+"/"+string(state), func(t *testing.T) {
				if err := boundary.acknowledge(t.Context(), receipt); !errors.Is(err, frame.ErrInvalid) {
					t.Fatalf("invalid receipt reached an external control effect: %v", err)
				}
			})
		}
	}
}

func TestInvalidLaunchIsRejectedBeforeStartingDaemon(t *testing.T) {
	_, daemon := unavailableControls(t)
	request := LaunchRequest{AttemptRef: AttemptRef{AttemptID: "attempt", RunnerID: "runner"}}
	result, err := daemon.Launch(t.Context(), request)
	if !errors.Is(err, frame.ErrInvalid) || !result.NotDispatched(request) {
		t.Fatalf("invalid launch reached daemon startup: %+v %v", result, err)
	}
}

func unavailableControls(t *testing.T) (*Client, *Daemon) {
	t.Helper()
	directory := listenerDirectory(t)
	client, err := NewClient(filepath.Join(directory, "missing.sock"))
	if err != nil {
		t.Fatal(err)
	}
	daemon, err := NewDaemon(DaemonConfig{Executable: filepath.Join(directory, "missing-runner"), Directory: directory, StartTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := daemon.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return client, daemon
}

type attemptControls interface {
	Fence(context.Context, uint64) error
	Inspect(context.Context, string) (Observation, error)
	CloseAttempt(context.Context, AttemptRef) (Observation, error)
	ProveAbsent(context.Context, AttemptRef) (AbsenceProof, error)
	Attach(context.Context, AttemptRef) (io.ReadWriteCloser, error)
}

func TestInvalidAttemptControlsAreRejectedBeforeConnecting(t *testing.T) {
	client, daemon := unavailableControls(t)
	ref := AttemptRef{AttemptID: "attempt", RunnerID: "runner"}
	for _, endpoint := range []struct {
		name     string
		controls attemptControls
	}{{"client", client}, {"daemon", daemon}} {
		for _, operation := range []struct {
			name string
			call func(context.Context) error
		}{
			{"fence", func(ctx context.Context) error { return endpoint.controls.Fence(ctx, 0) }},
			{"inspect", func(ctx context.Context) error { _, err := endpoint.controls.Inspect(ctx, ""); return err }},
			{"close", func(ctx context.Context) error { _, err := endpoint.controls.CloseAttempt(ctx, ref); return err }},
			{"absence", func(ctx context.Context) error { _, err := endpoint.controls.ProveAbsent(ctx, ref); return err }},
			{"attach", func(ctx context.Context) error {
				stream, err := endpoint.controls.Attach(ctx, ref)
				if stream != nil {
					_ = stream.Close()
				}
				return err
			}},
		} {
			t.Run(endpoint.name+"/"+operation.name, func(t *testing.T) {
				if err := operation.call(t.Context()); !errors.Is(err, frame.ErrInvalid) {
					t.Fatalf("invalid attempt control reached an external effect: %v", err)
				}
			})
		}
	}
}

func TestControlDecoderRejectsInvalidOperationIdentities(t *testing.T) {
	ref := AttemptRef{AttemptID: "attempt", RunnerID: "runner"}
	for _, request := range []controlRequest{
		fenceRequest{}, inspectRequest{}, launchControlRequest{}, closeRequest{ref}, proveAbsentRequest{ref},
		acknowledgeExitRequest{ExitReceipt{AttemptRef: ref, State: NotCreated, ProofID: "proof"}},
		attachControlRequest{ref}, attachStreamRequest{ref},
	} {
		t.Run(string(request.method()), func(t *testing.T) {
			params, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(controlRequestWire{Operation: request.method(), Params: params})
			if err != nil {
				t.Fatal(err)
			}
			if accepted, err := readRequest(bufio.NewReader(bytes.NewReader(append(data, '\n')))); !errors.Is(err, frame.ErrInvalid) || accepted != nil {
				t.Fatalf("invalid wire identity acquired a control request: %+v %v", accepted, err)
			}
		})
	}
}
