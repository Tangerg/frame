package runner

import (
	"bufio"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestFailedKillRetainsOriginalCloseAndAuthority(t *testing.T) {
	for _, closeServer := range []bool{false, true} {
		name := "attempt"
		if closeServer {
			name = "server"
		}
		t.Run(name, func(t *testing.T) {
			server, client, ctx := serveFixture(t)
			if err := client.Fence(ctx, 1); err != nil {
				t.Fatal(err)
			}
			request := launchFixture(t, client, "original-kill", 1, t.TempDir())
			for i, entry := range request.Env {
				if strings.HasPrefix(entry, "FRAME_RUNNER_HELPER=") {
					request.Env[i] = "FRAME_RUNNER_HELPER=wait-for-kill"
				}
			}
			if _, err := client.Launch(ctx, request); err != nil {
				t.Fatal(err)
			}
			stream, err := client.Attach(ctx, request.AttemptRef)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			if ready, err := bufio.NewReader(stream).ReadString('\n'); err != nil || ready != "ready\n" {
				t.Fatalf("original child was not ready: %q %v", ready, err)
			}
			failure := errors.New("original kill was rejected")
			var signals atomic.Int32
			server.mu.Lock()
			live := server.processes[request.AttemptID]
			originalKill := live.kill
			live.kill = func() error {
				if signals.Add(1) == 1 {
					return failure
				}
				return originalKill()
			}
			server.mu.Unlock()
			closeOriginal := func() error {
				if closeServer {
					return server.Close()
				}
				_, err := server.CloseAttempt(ctx, request.AttemptRef)
				return err
			}
			if err := closeOriginal(); !errors.Is(err, failure) {
				t.Fatalf("original failed signal was hidden: %v", err)
			}
			if lock, err := lockDirectory(filepath.Join(server.config.Directory, "authority.lock")); err == nil {
				_ = unlockDirectory(lock)
				t.Fatal("failed termination relinquished the original authority")
			}
			select {
			case <-live.done:
				t.Fatal("failed signal fabricated original exit")
			default:
			}
			if err := closeOriginal(); err != nil {
				t.Fatalf("original close could not advance after a rejected signal: %v", err)
			}
			if signals.Load() != 2 {
				t.Fatalf("close did not advance its original OS handle: signals=%d", signals.Load())
			}
			observation, err := server.Inspect(context.Background(), request.AttemptID)
			if err != nil || observation.State != Exited || observation.ProofID == "" {
				t.Fatalf("original Wait did not provide exit evidence: %+v %v", observation, err)
			}
			if err := server.Close(); err != nil {
				t.Fatal(err)
			}
			lock, err := lockDirectory(filepath.Join(server.config.Directory, "authority.lock"))
			if err != nil {
				t.Fatalf("confirmed original release retained authority: %v", err)
			}
			if err := unlockDirectory(lock); err != nil {
				t.Fatal(err)
			}
		})
	}
}
