package mcp

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"io"
	"testing"
	"testing/iotest"

	"github.com/Tangerg/frame"
)

type frameRead struct {
	frames [][]byte
	err    error
}

func readFrames(channel io.Reader, maximum int) frameRead {
	reader := newFrameReader(io.NopCloser(channel), maximum)
	var read frameRead
	for {
		data, err := reader.readFrame()
		if err != nil {
			read.err = err
			return read
		}
		read.frames = append(read.frames, bytes.Clone(data))
	}
}

func frameErrorClass(err error) error {
	for _, class := range []error{io.EOF, io.ErrUnexpectedEOF, frame.ErrCapacity, frame.ErrContractMismatch} {
		if errors.Is(err, class) {
			return class
		}
	}
	return nil
}

func FuzzFrameReaderAdmitsOnlyCompleteBoundedValues(f *testing.F) {
	for _, seed := range []string{
		"", "\n", "{}\n", "{}\n[]\n", `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n",
		`{"a":1,"a":2}` + "\n", "{\n}\n", "{}", "{} {}\n", "\"\xff\"\n",
		`{"padding":"` + string(bytes.Repeat([]byte("x"), 80)) + `"}` + "\n",
	} {
		f.Add([]byte(seed))
	}
	const maximum = 64
	f.Fuzz(func(t *testing.T, data []byte) {
		whole := readFrames(bytes.NewReader(data), maximum)
		split := readFrames(iotest.OneByteReader(bytes.NewReader(data)), maximum)
		class := frameErrorClass(whole.err)
		if class == nil {
			t.Fatalf("unclassified frame error: %v", whole.err)
		}
		if class != frameErrorClass(split.err) || len(whole.frames) != len(split.frames) {
			t.Fatalf("read boundaries changed admission: whole=%d %v split=%d %v", len(whole.frames), whole.err, len(split.frames), split.err)
		}
		for i, admitted := range whole.frames {
			if !bytes.Equal(admitted, split.frames[i]) {
				t.Fatalf("frame %d differs across read boundaries: %q != %q", i, admitted, split.frames[i])
			}
			if len(admitted) > maximum || admitted[len(admitted)-1] != '\n' || !jsontext.Value(admitted).IsValid() {
				t.Fatalf("admitted frame %q is not one bounded, complete JSON line", admitted)
			}
		}
	})
}
