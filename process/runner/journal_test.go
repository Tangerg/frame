package runner

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func interruptJournal(t *testing.T, server *Server) func() {
	t.Helper()
	server.mu.Lock()
	defer server.mu.Unlock()
	path := filepath.Join(server.config.Directory, "attempts.json")
	original := path + ".original"
	if err := os.Rename(path, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		_ = os.Rename(original, path)
		t.Fatal(err)
	}
	restore := sync.OnceFunc(func() {
		server.mu.Lock()
		defer server.mu.Unlock()
		if err := os.Remove(path); err != nil {
			t.Error(err)
			return
		}
		if err := os.Rename(original, path); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(restore)
	return restore
}

func TestWaitEvidenceSurvivesJournalFailureAndCompletesHandoff(t *testing.T) {
	server, client, ctx := serveFixture(t)
	if err := client.Fence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	request := launchFixture(t, client, "recover-original-wait", 1, t.TempDir())
	if _, err := client.Launch(ctx, request); err != nil {
		t.Fatal(err)
	}
	stream, err := client.Attach(ctx, request.AttemptRef)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stream.Close() })
	if _, err := io.WriteString(stream, "ready\n"); err != nil {
		t.Fatal(err)
	}
	if ready, err := bufio.NewReader(stream).ReadString('\n'); err != nil || ready != "ready\n" {
		t.Fatalf("original child readiness: %q %v", ready, err)
	}
	server.mu.Lock()
	live := server.processes[request.AttemptID]
	server.mu.Unlock()
	restore := interruptJournal(t, server)
	first, err := server.CloseAttempt(ctx, request.AttemptRef)
	if !errors.Is(err, ErrUnknown) || first.State != Unknown || first.ProofID != "" || first.ExitCode != nil {
		t.Fatalf("uncommitted exit exposed proof: %+v %v", first, err)
	}
	<-live.done
	if live.command.ProcessState == nil {
		t.Fatal("original Wait has not observed exit")
	}
	server.mu.Lock()
	original := server.terminalObservations[request.AttemptID].clone()
	server.mu.Unlock()
	if original.State != Exited || original.ProofID == "" || original.ExitCode == nil {
		t.Fatalf("original Wait fact was lost: %+v", original)
	}
	if observed, err := client.Inspect(ctx, request.AttemptID); !errors.Is(err, ErrUnknown) || observed.ProofID != "" || observed.ExitCode != nil {
		t.Fatalf("inspection exposed uncommitted evidence: %+v %v", observed, err)
	}
	restore()
	settled, err := client.CloseAttempt(ctx, request.AttemptRef)
	if err != nil || settled.State != Exited || settled.ProofID != original.ProofID || settled.ExitCode == nil || *settled.ExitCode != *original.ExitCode {
		t.Fatalf("storage recovery changed original Wait: %+v %v", settled, err)
	}
	identity, err := client.Identity(ctx)
	if err != nil || !identity.Fenced || identity.Epoch != request.Epoch {
		t.Fatalf("evidence recovery changed creation authority: %+v %v", identity, err)
	}
	receipt := ExitReceipt{AttemptID: settled.AttemptID, Epoch: settled.Epoch, RunnerID: settled.RunnerID, ProofID: settled.ProofID}
	if err := client.AcknowledgeExit(ctx, receipt); err != nil {
		t.Fatalf("creation fence prevented original exit handoff: %v", err)
	}
	if replayed, err := client.Launch(ctx, request); err != nil || replayed.ProofID != original.ProofID {
		t.Fatalf("replay did not retain original evidence: %+v %v", replayed, err)
	}
	count, err := os.ReadFile(filepath.Join(request.Directory, "created"))
	if err != nil || string(count) != "created\n" {
		t.Fatalf("evidence recovery recreated the child: %q %v", count, err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := Open(server.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = replacement.Close() })
	if persisted, err := replacement.Inspect(ctx, request.AttemptID); err != nil || persisted.ProofID != original.ProofID {
		t.Fatalf("restart lost original receipt: %+v %v", persisted, err)
	}
}

func TestCloseRetainsAuthorityUntilOriginalWaitIsDurable(t *testing.T) {
	server, client, ctx := serveFixture(t)
	if err := client.Fence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	request := launchFixture(t, client, "close-with-pending-evidence", 1, t.TempDir())
	if _, err := client.Launch(ctx, request); err != nil {
		t.Fatal(err)
	}
	restore := interruptJournal(t, server)
	if err := server.Close(); !errors.Is(err, ErrUnknown) {
		t.Fatalf("uncommitted Wait completed shutdown: %v", err)
	}
	if lock, err := lockDirectory(filepath.Join(server.config.Directory, "authority.lock")); err == nil {
		_ = unlockDirectory(lock)
		t.Fatal("uncommitted Wait abandoned its original authority")
	}
	server.mu.Lock()
	original := server.terminalObservations[request.AttemptID].clone()
	server.mu.Unlock()
	if original.State != Exited || original.ProofID == "" {
		t.Fatalf("shutdown lost original Wait: %+v", original)
	}
	restore()
	if err := server.Close(); err != nil {
		t.Fatalf("shutdown could not commit the original Wait: %v", err)
	}
	replacement, err := Open(server.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = replacement.Close() })
	if observation, err := replacement.Inspect(ctx, request.AttemptID); err != nil || observation.State != Exited || observation.ProofID != original.ProofID {
		t.Fatalf("replacement changed the original proof: %+v %v", observation, err)
	}
}

func TestReservationFailureRetainsOriginalNonCreationEvidence(t *testing.T) {
	server, err := Open(Config{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	ctx := t.Context()
	if err := server.Fence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	request := launchFixture(t, server, "failed-reservation", 1, t.TempDir())
	restore := interruptJournal(t, server)
	initial, err := server.Launch(ctx, request)
	if !errors.Is(err, ErrUnknown) || initial.State != Unknown || initial.ProofID != "" {
		t.Fatalf("uncommitted non-creation exposed proof: %+v %v", initial, err)
	}
	server.mu.Lock()
	original := server.terminalObservations[request.AttemptID].clone()
	live := len(server.processes)
	server.mu.Unlock()
	if original.State != Exited || original.ProofID == "" || original.ExitCode != nil || live != 0 {
		t.Fatalf("reservation failure lost its non-creation fact: %+v, live=%d", original, live)
	}
	if err := server.Close(); !errors.Is(err, ErrUnknown) {
		t.Fatalf("uncommitted non-creation completed shutdown: %v", err)
	}
	if lock, err := lockDirectory(filepath.Join(server.config.Directory, "authority.lock")); err == nil {
		_ = unlockDirectory(lock)
		t.Fatal("uncommitted non-creation abandoned its original authority")
	}
	restore()
	settled, err := server.CloseAttempt(ctx, request.AttemptRef)
	if err != nil || settled.State != Exited || settled.ProofID != original.ProofID || settled.ExitCode != nil {
		t.Fatalf("storage recovery changed original non-creation evidence: %+v %v", settled, err)
	}
	if replayed, err := server.Launch(ctx, request); err != nil || replayed.ProofID != original.ProofID {
		t.Fatalf("replay changed the refused start: %+v %v", replayed, err)
	}
	if len(executableSnapshots(t, server)) != 0 {
		t.Fatal("failed reservation reached executable preparation")
	}
	if _, err := os.Stat(filepath.Join(request.Directory, "created")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed reservation created a child: %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := Open(server.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = replacement.Close() })
	if recovered, err := replacement.Inspect(ctx, request.AttemptID); err != nil || recovered.State != Exited || recovered.ProofID != original.ProofID {
		t.Fatalf("authority transfer lost original non-creation evidence: %+v %v", recovered, err)
	}
	receipt := ExitReceipt{AttemptID: settled.AttemptID, Epoch: settled.Epoch, RunnerID: settled.RunnerID, ProofID: settled.ProofID}
	if err := replacement.AcknowledgeExit(ctx, receipt); err != nil {
		t.Fatalf("original non-creation handoff: %v", err)
	}
}
