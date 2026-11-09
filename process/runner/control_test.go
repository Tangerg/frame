package runner

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/Tangerg/frame"
)

func TestControlRequestLimitIncludesLineFeed(t *testing.T) {
	req := inspectRequest{AttemptID: "x"}
	encoded, err := encodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	req.AttemptID += strings.Repeat("x", maxControlBytes-len(encoded))
	var wire bytes.Buffer
	if err = writeRequest(&wire, req); err != nil {
		t.Fatal(err)
	}
	got, err := readLine(bufio.NewReader(&wire))
	if err != nil || len(got) != maxControlBytes {
		t.Fatalf("accepted wire size %d: %v", len(got), err)
	}
	req.AttemptID += "x"
	if err = writeRequest(&wire, req); !errors.Is(err, frame.ErrCapacity) || wire.Len() != 0 {
		t.Fatalf("oversized request sent: %d, %v", wire.Len(), err)
	}
}

func TestRunnerCannotServeAnUnrestrictedNetworkListener(t *testing.T) {
	server, err := Open(Config{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err = server.Serve(t.Context(), listener); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("unsupported listener: %v", err)
	}
}
