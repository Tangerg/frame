package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Tangerg/frame/examples/document"
	"github.com/Tangerg/frame/internal/localfile"
)

func TestRejectedCommandsCannotCreateApplicationState(t *testing.T) {
	for _, args := range [][]string{
		{"-command", "unknown"},
		{"-command", "run"},
		{"-command", "status"},
		{"-command", "generate"},
		{"-unknown"},
		{"unexpected"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "unopened")
			args := append([]string{"-state", state, "-documents", t.TempDir()}, args...)
			if err := run(t.Context(), args, io.Discard); err == nil {
				t.Fatal("invalid command was admitted")
			}
			if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected command opened application state: %v", err)
			}
		})
	}
}

func TestGenerateDoesNotOpenTheApplication(t *testing.T) {
	root := t.TempDir()
	state, documents := filepath.Join(root, "state"), filepath.Join(root, "documents")
	target := filepath.Join(root, "api.ts")
	if err := run(t.Context(), []string{
		"-command", "generate", "-out", target, "-state", state, "-documents", documents,
	}, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil || len(data) == 0 {
		t.Fatalf("API projection was not written: %v", err)
	}
	for _, path := range []string{state, documents} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("generation opened the application at %s: %v", path, err)
		}
	}
}

func TestEachInvocationOwnsItsFlags(t *testing.T) {
	for range 2 {
		var output bytes.Buffer
		if err := run(t.Context(), []string{"-help"}, &output); !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("help failed: %v", err)
		}
		if !strings.Contains(output.String(), "-command") {
			t.Fatal("help omitted command usage")
		}
	}
}

func TestCancelledInvocationCannotOpenTheApplication(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	state := filepath.Join(t.TempDir(), "unopened")
	err := run(ctx, []string{"-command", "queue", "-state", state}, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was lost: %v", err)
	}
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled command opened application state: %v", err)
	}
}

func TestCommandsObserveTheOriginalTaskAcrossReopening(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() {
		if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return os.Chmod(path, localfile.PrivateDirectoryMode)
			}
			return nil
		}); err != nil {
			t.Error(err)
		}
	})
	documents := filepath.Join(root, "documents")
	if err := os.Mkdir(documents, localfile.PrivateDirectoryMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(documents, "hello.md"), []byte("# Original\nHello\n"), localfile.PrivateFileMode); err != nil {
		t.Fatal(err)
	}
	arguments := []string{
		"-state", filepath.Join(root, "state"), "-documents", documents,
		"-document", "hello.md", "-task", "original-task",
	}
	var original *document.TaskResult
	for _, sample := range []struct {
		command command
		state   document.TaskState
	}{
		{commandQueue, document.Queued},
		{commandStatus, document.Queued},
		{commandRun, document.Completed},
		{commandStatus, document.Completed},
		{commandConvert, document.Completed},
	} {
		var output bytes.Buffer
		args := append(append([]string(nil), arguments...), "-command", string(sample.command))
		if err := run(t.Context(), args, &output); err != nil {
			t.Fatal(err)
		}
		var task document.Task
		if err := json.Unmarshal(output.Bytes(), &task); err != nil {
			t.Fatal(err)
		}
		if task.Definition.ID != "original-task" || task.State != sample.state {
			t.Fatalf("%s changed task identity or state: %#v", sample.command, task)
		}
		if sample.command == commandRun {
			original = task.Result
		}
		if sample.state == document.Completed && !reflect.DeepEqual(task.Result, original) {
			t.Fatalf("%s replaced the original result", sample.command)
		}
	}
}
