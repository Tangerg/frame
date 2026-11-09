package runner

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tangerg/frame"
)

func capacityFixture(t *testing.T) (*Server, LaunchRequest) {
	t.Helper()
	directory, err := os.MkdirTemp("", "frame-launch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	server, err := Open(Config{Directory: directory, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := server.Fence(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	first := launchFixture(t, server, "occupied", 1, t.TempDir())
	if _, err := server.Launch(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := server.CloseAttempt(t.Context(), first.AttemptRef); err != nil {
		t.Fatal(err)
	}
	request := launchFixture(t, server, "refused", 1, t.TempDir())
	return server, request
}

func TestCapacityRefusalHasAnOriginalDurableReceiptWithoutAnActiveSlot(t *testing.T) {
	server, request := capacityFixture(t)
	host, endpoint := net.Pipe()
	t.Cleanup(func() { _ = host.Close() })
	done := make(chan struct{})
	go func() { defer close(done); defer endpoint.Close(); server.handle(t.Context(), endpoint, func() {}) }()
	data, err := encodeRequest(launchControlRequest{request})
	if err != nil {
		t.Fatal(err)
	}
	response, dispatch, err := exchangeControl(t.Context(), host, methodLaunch, data)
	if !errors.Is(err, frame.ErrCapacity) || dispatch != controlDispatchUnknown {
		t.Fatalf("wire refusal: %+v %v", response, err)
	}
	<-done
	original, ok := response.(Observation)
	if !ok || original.State != NotCreated || original.Reference() != request.AttemptRef || original.ProofID == "" {
		t.Fatalf("missing original non-creation proof: %+v", response)
	}
	if len(server.journal.Attempts) != 1 || len(server.terminalObservations) != 0 {
		t.Fatal("refusal consumed active capacity")
	}
	occupied, err := server.Inspect(t.Context(), "occupied")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := occupied.ExitReceipt()
	if err != nil {
		t.Fatal(err)
	}
	if err := server.AcknowledgeExit(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	if result, err := server.Launch(t.Context(), request); err != nil || result.State != NotCreated || result.ProofID != original.ProofID || result.NotDispatched(request) {
		t.Fatalf("refusal identity was recreated: %+v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(request.Directory, "created")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused attempt created a child: %v", err)
	}
	conflict := request
	conflict.Args = append(conflict.Args, "different")
	if _, err := server.Launch(t.Context(), conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("refusal failed to bind original request: %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := Open(server.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := replacement.Close(); err != nil {
			t.Error(err)
		}
	})
	if result, err := replacement.Launch(t.Context(), request); err != nil || result.State != NotCreated || result.ProofID != original.ProofID {
		t.Fatalf("restart changed refusal: %+v %v", result, err)
	}
}

func TestCapacityRefusalWithUnavailableReceiptStorageRemainsUnknown(t *testing.T) {
	server, request := capacityFixture(t)
	if err := os.Symlink("unavailable-receipt-parent", filepath.Join(server.config.Directory, receiptDirectory)); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(filepath.Join(server.config.Directory, receiptDirectory))
	result, err := server.Launch(t.Context(), request)
	if !errors.Is(err, ErrUnknown) || !errors.Is(err, frame.ErrCapacity) || result.State != Unknown || result.ProofID != "" || result.NotDispatched(request) {
		t.Fatalf("undurable refusal became release evidence: %+v %v", result, err)
	}
	original := server.terminalObservations[request.AttemptID]
	if original.State != NotCreated || original.ProofID == "" || !server.fenced {
		t.Fatal("original refusal owner was lost")
	}
	if observed, err := server.CloseAttempt(t.Context(), request.AttemptRef); !errors.Is(err, ErrUnknown) || observed.ProofID != "" {
		t.Fatalf("storage failure leaked release proof: %+v %v", observed, err)
	}
	if err := os.Remove(filepath.Join(server.config.Directory, receiptDirectory)); err != nil {
		t.Fatal(err)
	}
	settled, err := server.CloseAttempt(t.Context(), request.AttemptRef)
	if err != nil || settled.State != NotCreated || settled.ProofID != original.ProofID {
		t.Fatalf("restored storage replaced the original refusal: %+v %v", settled, err)
	}
	if len(server.journal.Attempts) != 1 || len(server.terminalObservations) != 0 {
		t.Fatal("restored refusal retained an active slot")
	}
}

func TestLaunchNonDispatchEvidenceBindsOnlyTheOriginalInvocation(t *testing.T) {
	server, request := capacityFixture(t)
	cause := errors.New("original caller cancelled")
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(cause)
	client, err := NewClient(filepath.Join(server.config.Directory, "missing-socket"))
	if err != nil {
		t.Fatal(err)
	}
	for _, authority := range []interface {
		Launch(context.Context, LaunchRequest) (LaunchResult, error)
	}{server, client} {
		result, err := authority.Launch(ctx, request)
		if !result.NotDispatched(request) || !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
			t.Fatalf("original preflight: %+v %v", result, err)
		}
		changed := request
		changed.Env = []string{"TOKEN=different"}
		if result.NotDispatched(changed) || (LaunchResult{}).NotDispatched(request) {
			t.Fatal("nondispatch proof accepted a different or unknown request")
		}
		result.Observation = Observation{AttemptID: request.AttemptID}
		if result.NotDispatched(request) {
			t.Fatal("contradictory observation retained nondispatch authority")
		}
	}
	oversized := request
	oversized.Args = []string{strings.Repeat("x", maxControlBytes)}
	if result, err := client.Launch(t.Context(), oversized); !result.NotDispatched(oversized) || !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("encoding refusal lost exact nondispatch evidence: %+v %v", result, err)
	}
	if result, err := client.Launch(t.Context(), request); !result.NotDispatched(request) || err == nil {
		t.Fatalf("failed connection lost exact nondispatch evidence: %+v %v", result, err)
	}
}

func TestControlWriteWithoutReplyCannotProveNonDispatch(t *testing.T) {
	_, request := capacityFixture(t)
	host, endpoint := net.Pipe()
	defer host.Close()
	go func() { defer endpoint.Close(); data := make([]byte, 1); _, _ = endpoint.Read(data) }()
	data, err := encodeRequest(launchControlRequest{request})
	if err != nil {
		t.Fatal(err)
	}
	response, dispatch, err := exchangeControl(t.Context(), host, methodLaunch, data)
	if err == nil || dispatch != controlDispatchUnknown || response != nil {
		t.Fatalf("partial write became nondispatch: %+v dispatch=%d error=%v", response, dispatch, err)
	}
}

func TestDurableNonCreationReceiptSurvivesAnInterruptedJournalProjection(t *testing.T) {
	server, request := capacityFixture(t)
	occupied, err := server.Inspect(t.Context(), "occupied")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := occupied.ExitReceipt()
	if err != nil {
		t.Fatal(err)
	}
	if err := server.AcknowledgeExit(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	digest, err := requestIdentity(request)
	if err != nil {
		t.Fatal(err)
	}
	server.journal.Attempts[request.AttemptID] = storedAttempt{Observation: Observation{AttemptID: request.AttemptID, Epoch: request.Epoch, RunnerID: request.RunnerID, State: Starting}, RequestDigest: digest}
	if err := server.persist(); err != nil {
		t.Fatal(err)
	}
	restore := interruptJournal(t, server)
	server.mu.Lock()
	result, err := server.failedStart(request.AttemptID, "creation refused", frame.ErrCapacity)
	original := server.terminalObservations[request.AttemptID]
	server.mu.Unlock()
	if !errors.Is(err, ErrUnknown) || result.ProofID != "" {
		t.Fatalf("journal failure exposed receipt: %+v %v", result, err)
	}
	restore()
	loaded, err := loadJournal(server.directory, server.config.MaxAttempts)
	if err != nil {
		t.Fatal(err)
	}
	projection := &Server{config: server.config, directory: server.directory, journal: loaded}
	if err := projection.restoreReceipts(); err != nil {
		t.Fatal(err)
	}
	recovered, found, err := projection.lookupAttempt(request.AttemptID)
	if err != nil || !found || recovered.Observation.State != NotCreated || recovered.Observation.ProofID != original.ProofID {
		t.Fatalf("disk reconstruction lost terminal receipt: %+v %v", recovered, err)
	}
}
