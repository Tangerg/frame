package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/distribution"
	framehost "github.com/Tangerg/frame/host"
)

func TestCommandUsageErrorsHaveInputCategory(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "missing command"},
		{name: "unknown command", args: []string{"unknown"}},
		{name: "contract flags", args: []string{"contract", "--unknown"}},
		{name: "missing feature", args: []string{"contract"}},
		{name: "package flags", args: []string{"package", "--unknown"}},
		{name: "missing package input", args: []string{"package"}},
		{name: "missing archive", args: []string{"inspect"}},
		{name: "archive flags", args: []string{"inspect", "archive.zip", "--unknown"}},
		{name: "report format", args: []string{"inspect", "archive.zip", "--format", "unknown"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := run(test.args)
			if !errors.Is(err, frame.ErrInvalid) || classifyDiagnostic(err) != diagnosticInvalidRequest {
				t.Fatalf("usage failure lost its input category: %v", err)
			}
		})
	}
}

type failedOutput struct{ cause error }

func (w failedOutput) Write([]byte) (int, error) { return 0, w.cause }

func TestArchiveOutputFailurePreservesOperationalCause(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "theme.css"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := distribution.Manifest{
		SchemaVersion: distribution.ManifestSchemaVersion,
		Package:       "theme",
		Version:       "1",
		Artifacts:     []distribution.Artifact{{Path: "theme.css"}},
		Units:         []distribution.Unit{{ID: "theme", Profile: framehost.StaticResource, Resources: []string{"theme.css"}}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, distribution.ManifestName), data, 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "theme.zip")
	if err := runPackage([]string{"--source", source, "--out", archive}, io.Discard); err != nil {
		t.Fatal(err)
	}
	packageOutput := filepath.Join(t.TempDir(), "theme.zip")
	cause := errors.New("output storage unavailable")
	for _, operation := range []struct {
		name string
		run  func() error
	}{
		{name: "package", run: func() error {
			return runPackage([]string{"--source", source, "--out", packageOutput}, failedOutput{cause: cause})
		}},
		{name: "inspect", run: func() error { return runInspect([]string{archive}, failedOutput{cause: cause}) }},
	} {
		t.Run(operation.name, func(t *testing.T) {
			err := operation.run()
			if !errors.Is(err, cause) || classifyDiagnostic(err) != diagnosticOperationFailed {
				t.Fatalf("output failure changed its cause or became an input error: %v", err)
			}
		})
	}
}

func TestInvalidArchiveRetainsInputCategory(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "invalid.zip")
	if err := os.WriteFile(archive, []byte("invalid archive"), 0600); err != nil {
		t.Fatal(err)
	}
	err := runInspect([]string{archive}, failedOutput{cause: errors.New("unexpected output")})
	if !errors.Is(err, distribution.ErrInvalid) || classifyDiagnostic(err) != diagnosticInvalidRequest {
		t.Fatalf("invalid archive lost its input category: %v", err)
	}
}
