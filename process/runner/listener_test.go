package runner

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Tangerg/frame"
)

func listenerDirectory(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "frame-listener-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}

func openListenerServer(t *testing.T, directory string) *Server {
	t.Helper()
	server, err := Open(Config{Directory: directory})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
	})
	return server
}

func assertSocketIdentity(t *testing.T, path string, original os.FileInfo) {
	t.Helper()
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(original, current) {
		t.Fatalf("original socket was replaced: %v", err)
	}
}

func TestClosedServerAndListenerCannotReplaceSuccessorSocket(t *testing.T) {
	directory := listenerDirectory(t)
	first := openListenerServer(t, directory)
	path := filepath.Join(directory, "socket")
	old, err := first.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed authority left its listener open: %v", err)
	}
	next := openListenerServer(t, directory)
	if _, err := next.Listen(path); err != nil {
		t.Fatal(err)
	}
	original, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if stale, err := first.Listen(path); !errors.Is(err, frame.ErrClosed) {
		if stale != nil {
			_ = stale.Close()
		}
		t.Fatalf("closed authority published an endpoint: %v", err)
	}
	assertSocketIdentity(t, path, original)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	assertSocketIdentity(t, path, original)
}

func TestListenerPublicationHasOneActiveOwner(t *testing.T) {
	directory := listenerDirectory(t)
	server := openListenerServer(t, directory)
	path := filepath.Join(directory, "socket")
	first, err := server.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := server.Listen(path); !errors.Is(err, ErrConflict) {
		if duplicate != nil {
			_ = duplicate.Close()
		}
		t.Fatalf("duplicate publication: %v", err)
	}
	assertSocketIdentity(t, path, original)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Listen(path); err != nil {
		t.Fatalf("released publication retained its slot: %v", err)
	}
	replacement, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	assertSocketIdentity(t, path, replacement)
}

func TestListenerCloseDoesNotUnlinkAReplacedPath(t *testing.T) {
	directory := listenerDirectory(t)
	server := openListenerServer(t, directory)
	path := filepath.Join(directory, "socket")
	listener, err := server.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("another owner's file"), 0600); err != nil {
		t.Fatal(err)
	}
	original, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	assertSocketIdentity(t, path, original)
}

func TestListenAndCloseSerializeDirectoryAuthority(t *testing.T) {
	directory := listenerDirectory(t)
	server := openListenerServer(t, directory)
	path := filepath.Join(directory, "socket")
	start := make(chan struct{})
	var listener net.Listener
	var listenErr, closeErr error
	var concurrent sync.WaitGroup
	concurrent.Go(func() { <-start; listener, listenErr = server.Listen(path) })
	concurrent.Go(func() { <-start; closeErr = server.Close() })
	close(start)
	concurrent.Wait()
	if closeErr != nil || (listenErr != nil && !errors.Is(listenErr, frame.ErrClosed)) {
		t.Fatalf("concurrent authority transitions: listen=%v close=%v", listenErr, closeErr)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("closed authority retained a published socket: %v", err)
	}
	replacement := openListenerServer(t, directory)
	if _, err := replacement.Listen(path); err != nil {
		t.Fatal(err)
	}
	original, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if listener != nil {
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
	}
	assertSocketIdentity(t, path, original)
}

func TestServerCloseJoinsItsAcceptedConnections(t *testing.T) {
	server, client, ctx := serveFixture(t)
	connection, err := client.dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if _, err := client.Identity(ctx); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("shutdown did not close its accepted connection: %v", err)
	}
	server.mu.Lock()
	served := server.listener.served
	server.mu.Unlock()
	select {
	case <-served:
	default:
		t.Fatal("directory authority was released before Serve joined")
	}
}

func TestServeRefusesAnUnownedUnixListener(t *testing.T) {
	directory := listenerDirectory(t)
	server := openListenerServer(t, directory)
	listener, err := net.Listen("unix", filepath.Join(directory, "foreign"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := server.Serve(context.Background(), listener); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("Serve adopted an independent publication owner: %v", err)
	}
}
