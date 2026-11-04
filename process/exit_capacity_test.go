package process_test

import (
	"bytes"
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/process"
	"github.com/Tangerg/frame/process/runner"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

type exitCapacityRunner struct {
	mu           sync.Mutex
	identity     runner.Identity
	observation  runner.Observation
	launches     int
	acknowledged []runner.ExitReceipt
}

func (r *exitCapacityRunner) Identity(context.Context) (runner.Identity, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.identity, nil
}

func (r *exitCapacityRunner) Fence(_ context.Context, epoch uint64) error {
	r.mu.Lock()
	r.identity.Epoch = epoch
	r.mu.Unlock()
	return nil
}

func (r *exitCapacityRunner) Launch(_ context.Context, request runner.LaunchRequest) (runner.Observation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.launches++
	r.observation.AttemptID = request.AttemptID
	r.observation.Epoch = request.Epoch
	r.observation.RunnerID = request.RunnerID
	r.observation.State = runner.Running
	return r.observation, nil
}

type emptyProcessStream struct{ bytes.Buffer }

func (*emptyProcessStream) Close() error { return nil }

func (*exitCapacityRunner) Attach(context.Context, runner.AttemptRef) (io.ReadWriteCloser, error) {
	return &emptyProcessStream{}, nil
}

func (r *exitCapacityRunner) Inspect(context.Context, string) (runner.Observation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.observation, nil
}

func (r *exitCapacityRunner) CloseAttempt(_ context.Context, ref runner.AttemptRef) (runner.Observation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.observation.Reference() != ref {
		return runner.Observation{}, runner.ErrUnknown
	}
	r.observation.State = runner.Exited
	return r.observation, nil
}

func (r *exitCapacityRunner) AcknowledgeExit(_ context.Context, receipt runner.ExitReceipt) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.observation.State != runner.Exited || r.observation.Reference() != (runner.AttemptRef{AttemptID: receipt.AttemptID, Epoch: receipt.Epoch, RunnerID: receipt.RunnerID}) || r.observation.ProofID != receipt.ProofID {
		return runner.ErrUnknown
	}
	r.acknowledged = append(r.acknowledged, receipt)
	return nil
}

func (*exitCapacityRunner) ProveAbsent(context.Context, runner.AttemptRef) (runner.AbsenceProof, error) {
	return runner.AbsenceProof{}, runner.ErrUnknown
}

type emptyProcessProgram struct{}

func (emptyProcessProgram) Program(context.Context, frame.UnitBinding) (process.Program, error) {
	return process.Program{}, nil
}

func exitCapacityHost(t *testing.T, requestBytes int, authority *exitCapacityRunner) (*frame.Host, *sqlite.Store, frame.UnitRef) {
	t.Helper()
	admission := frame.NewAdmission()
	store, err := sqlite.Open(t.Context(), sqlite.Config{Path: filepath.Join(t.TempDir(), "state.sqlite"), Admission: admission, MaxRequestBytes: requestBytes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	driver, err := process.New(process.Config{Runner: authority, Store: store, Source: emptyProcessProgram{}, CloseTimeout: time.Second, MaxFrameBytes: 4096, Protocol: cleanupProtocol{connection: &cleanupConnection{}}})
	if err != nil {
		t.Fatal(err)
	}
	host, err := frame.NewHost(t.Context(), frame.HostConfig{Admission: admission, Epoch: store.Epoch(), Drivers: []frame.Driver{driver}, Installations: store, ExecutionProtection: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if outcome, err := host.Close(ctx); !outcome.Released {
			t.Errorf("host release: %v", err)
		}
	})
	plugin, err := frame.Define(frame.Definition{Revision: value.Hash(nil), Units: []frame.Unit{{Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.LocalProcess}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.Install(t.Context(), frame.InstallationConfig{ID: "provider", Plugin: plugin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	return host, store, frame.UnitRef{InstallationID: "provider", Release: plugin.Revision(), UnitID: "main"}
}

func TestProcessRefusesUnsettleableExitBeforeApprovalOrLaunch(t *testing.T) {
	authority := &exitCapacityRunner{identity: runner.Identity{ID: "runner"}}
	host, store, unit := exitCapacityHost(t, 2048, authority)
	if _, err := host.ActivateUnit(t.Context(), "application", unit); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("exit capacity was not checked before admission: %v", err)
	}
	authority.mu.Lock()
	launches := authority.launches
	authority.mu.Unlock()
	if launches != 0 {
		t.Fatalf("capacity refusal dispatched %d processes", launches)
	}
	attempts, err := store.Attempts(t.Context())
	if err != nil || len(attempts) != 0 {
		t.Fatalf("capacity refusal accepted durable responsibility: %+v %v", attempts, err)
	}
}

func TestProcessExitReceiptFitsWithoutRunnerDiagnostics(t *testing.T) {
	const requestBytes = 8192
	authority := &exitCapacityRunner{
		identity: runner.Identity{ID: strings.Repeat("<", frame.MaxNameBytes)},
		observation: runner.Observation{
			ProofID: strings.Repeat("<", frame.MaxNameBytes),
			Reason:  strings.Repeat("diagnostic", requestBytes),
		},
	}
	host, store, unit := exitCapacityHost(t, requestBytes, authority)
	if _, err := host.ActivateUnit(t.Context(), "application", unit); err != nil {
		t.Fatal(err)
	}
	if outcome, err := host.Close(t.Context()); !outcome.Released || err != nil {
		t.Fatalf("runner diagnostics prevented original release: released=%t error=%v", outcome.Released, err)
	}
	attempts, err := store.Attempts(t.Context())
	if err != nil || len(attempts) != 1 || attempts[0].State != sqlite.AttemptReleased || attempts[0].Proof == nil {
		t.Fatalf("original exit was not durably acknowledged: %+v %v", attempts, err)
	}
	var receipt runner.ExitReceipt
	if err := jsonv2.Unmarshal(attempts[0].Proof.Evidence, &receipt, jsonv2.RejectUnknownMembers(true)); err != nil {
		t.Fatalf("exit evidence retained non-authoritative diagnostics: %v", err)
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if len(authority.acknowledged) != 1 || receipt != authority.acknowledged[0] || receipt.ProofID != authority.observation.ProofID {
		t.Fatalf("durable handoff replaced the original runner identity: %+v", receipt)
	}
	diagnostic, err := json.Marshal(authority.observation)
	if err != nil || len(diagnostic) <= requestBytes {
		t.Fatalf("fixture did not exceed durable proof capacity: bytes=%d error=%v", len(diagnostic), err)
	}
}
