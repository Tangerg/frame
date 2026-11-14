package process_test

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/process"
	"github.com/Tangerg/frame/process/runner"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

// A restart can replace the endpoint between two successful control calls.
// Each response here still comes from a real, independently owned runner.
type replaceAfterIdentity struct {
	process.Runner
	replacement process.Runner
}

func (r *replaceAfterIdentity) Identity(ctx context.Context) (runner.Identity, error) {
	identity, err := r.Runner.Identity(ctx)
	if err == nil && r.replacement != nil {
		r.Runner, r.replacement = r.replacement, nil
	}
	return identity, err
}

func recoveryRunner(t *testing.T, directory string) *runner.Client {
	t.Helper()
	server, err := runner.Open(runner.Config{Directory: directory})
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(directory, "socket")
	listener, err := server.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		<-done
		if err := server.Close(); err != nil {
			t.Error(err)
		}
	})
	client, err := runner.NewClient(socket)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestRecoveryRequiresOriginalSealedNonCreationEvidence(t *testing.T) {
	for _, scenario := range []string{"same-runner", "replacement-runner", "replacement-after-identity"} {
		t.Run(scenario, func(t *testing.T) {
			directory, err := os.MkdirTemp("", "frame-recovery-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(directory) })
			original := recoveryRunner(t, filepath.Join(directory, "original"))
			path := filepath.Join(directory, "state.sqlite")
			store, err := sqlite.Open(t.Context(), sqlite.Config{Path: path, Admission: framehost.NewAdmission()})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			if err := original.Fence(t.Context(), store.Epoch()); err != nil {
				t.Fatal(err)
			}
			identity, err := original.Identity(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			attempt := recordAttempt(t, store, identity)

			// The Host crashes after its durable approval and before Launch.
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = sqlite.Open(t.Context(), sqlite.Config{Path: path, Admission: framehost.NewAdmission()})
			if err != nil {
				t.Fatal(err)
			}
			var current process.Runner = original
			if scenario != "same-runner" {
				replacement := recoveryRunner(t, filepath.Join(directory, "replacement"))
				current = replacement
				if scenario == "replacement-after-identity" {
					current = &replaceAfterIdentity{Runner: original, replacement: replacement}
				}
			}
			source := &source{}
			driver, err := process.New(process.Config{Runner: current, Store: store, Source: source, CloseTimeout: time.Second, MaxFrameBytes: 1 << 20})
			if err != nil {
				t.Fatal(err)
			}
			err = driver.Recover(t.Context())
			if scenario == "same-runner" && err != nil {
				t.Fatalf("original sealed runner could not prove non-creation: %v", err)
			}
			if scenario != "same-runner" && !errors.Is(err, runner.ErrUnknown) {
				t.Fatalf("a different authority converted missing state into proof: %v", err)
			}
			records, err := store.Attempts(t.Context())
			if err != nil || len(records) != 1 || records[0].Attempt.ID != attempt.ID {
				t.Fatalf("original attempt changed: %+v %v", records, err)
			}
			dependencies, err := store.ExecutionDependencies(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "same-runner" {
				if !records[0].ExitProven() || records[0].Proof == nil || records[0].Proof.Kind != "sealed-not-started" || records[0].Proof.Realm != identity.ID || len(dependencies) != 0 {
					t.Fatalf("original non-creation evidence was not settled: %+v %+v", records, dependencies)
				}
			} else if records[0].ExitProven() || records[0].Proof != nil || len(dependencies) != 1 || dependencies[0] != attempt.Source.Dependency() {
				t.Fatalf("unknown execution lost its exact protection: %+v %+v", records, dependencies)
			}
			if source.programCalls.Load() != 0 {
				t.Fatal("recovery tried to recreate the approved attempt")
			}
			if _, err := original.Inspect(t.Context(), attempt.ID); !errors.Is(err, runner.ErrNotFound) {
				t.Fatalf("the original runner received a replacement creation: %v", err)
			}
		})
	}
}

func recordAttempt(t *testing.T, store sqlite.Store, identity runner.Identity) sqlite.Attempt {
	t.Helper()
	_, capability, err := echoContract()
	if err != nil {
		t.Fatal(err)
	}
	contract := framehost.ContractRef{Name: capability.Descriptor().Reference, Version: "1", Digest: capability.Identity().Digest(), Kind: framehost.ServiceKind, Mode: framehost.PortableMode}
	root := framehost.ExportRef{InstallationID: "original", Release: value.Hash([]byte("release")), UnitID: "backend", Export: "echo", Contract: contract}
	binding := framehost.BindingSnapshot{Roots: []framehost.ExportRef{root}, Units: []framehost.UnitBinding{{Source: root.Unit(), Configuration: value.Hash([]byte(`{}`)), Imports: []framehost.ImportBinding{}}}}
	attempt := sqlite.Attempt{ID: "approved-before-crash", Epoch: store.Epoch(), Source: root.Unit(), Binding: binding, Realm: identity.ID, Profile: framehost.LocalProcess}
	if err := store.RecordAttempt(t.Context(), attempt); err != nil {
		t.Fatal(err)
	}
	return attempt
}

func TestRecoverOriginalExitHandoffAcrossHostRestart(t *testing.T) {
	for _, alreadyAcknowledged := range []bool{false, true} {
		name := "needs-runner-ack"
		if alreadyAcknowledged {
			name = "needs-host-confirmation"
		}
		t.Run(name, func(t *testing.T) {
			directory, err := os.MkdirTemp("", "frame-receipt-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(directory) })
			ctx := t.Context()
			hostPath := filepath.Join(directory, "state.sqlite")
			open := func() sqlite.Store {
				store, err := sqlite.Open(ctx, sqlite.Config{Path: hostPath, Admission: framehost.NewAdmission()})
				if err != nil {
					t.Fatal(err)
				}
				return store
			}
			original := recoveryRunner(t, filepath.Join(directory, "runner"))
			store := open()
			if err := original.Fence(ctx, store.Epoch()); err != nil {
				t.Fatal(err)
			}
			identity, err := original.Identity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			attempt := recordAttempt(t, store, identity)
			req := runner.LaunchRequest{AttemptRef: runner.AttemptRef{AttemptID: attempt.ID, Epoch: attempt.Epoch, RunnerID: attempt.Realm}, Command: filepath.Join(directory, "missing"), ExecutableDigest: value.Hash([]byte("missing")), Configuration: attempt.Binding.Units[0].Configuration}
			exit, err := original.Launch(ctx, req)
			if err == nil || exit.State != runner.NotCreated {
				t.Fatalf("failed start: %+v %v", exit, err)
			}
			receipt, err := exit.ExitReceipt()
			if err != nil {
				t.Fatal(err)
			}
			evidence, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			proof := sqlite.ExitProof{AttemptID: attempt.ID, Realm: attempt.Realm, ExitID: exit.ProofID, Kind: "runner-exit", Evidence: evidence}
			if err := store.AcceptExit(ctx, attempt.ID, proof); err != nil {
				t.Fatal(err)
			}
			if alreadyAcknowledged {
				if err := original.AcknowledgeExit(ctx, receipt); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store = open()
			defer store.Close()
			driver, err := process.New(process.Config{Runner: original, Store: store, Source: &source{}, CloseTimeout: time.Second, MaxFrameBytes: 1 << 20})
			if err != nil {
				t.Fatal(err)
			}
			if err := driver.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			records, err := store.Attempts(ctx)
			if err != nil || len(records) != 1 || (records[0].State == sqlite.AttemptHandoff) || !records[0].ExitProven() || records[0].Proof.ExitID != exit.ProofID {
				t.Fatalf("unsettled original handoff: %+v %v", records, err)
			}
		})
	}
}
