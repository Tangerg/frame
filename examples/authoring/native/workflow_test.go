package native

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/examples/document"
	"github.com/Tangerg/frame/integrity"
)

func authorContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func authorFiles(t *testing.T) (Configuration, string, document.DocumentRef) {
	t.Helper()
	config := Configuration{SourceDirectory: t.TempDir(), WorkDirectory: t.TempDir()}
	template := []byte("<article>{{content}}</article>")
	if err := os.WriteFile(filepath.Join(config.SourceDirectory, "template.html"), template, 0600); err != nil {
		t.Fatal(err)
	}
	config.Template = document.DocumentRef{Path: "template.html", Revision: integrity.Hash(template)}
	text := []byte("Memo\nHello <world>\n")
	if err := os.WriteFile(filepath.Join(config.SourceDirectory, "memo.txt"), text, 0600); err != nil {
		t.Fatal(err)
	}
	return config, t.TempDir(), document.DocumentRef{Path: "memo.txt", Revision: integrity.Hash(text)}
}

func authorWorkbench(t *testing.T) (*Workbench, Configuration, string, document.DocumentRef, context.Context) {
	t.Helper()
	ctx := authorContext(t)
	config, output, reference := authorFiles(t)
	workbench, err := Open(ctx, config, output)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := workbench.Close(authorContext(t)); err != nil {
			t.Error(err)
		}
	})
	return workbench, config, output, reference, ctx
}

func authorOwner(t *testing.T) *frame.Lifetime {
	t.Helper()
	owner := frame.NewLifetime(context.Background())
	t.Cleanup(func() {
		if _, err := owner.Close(authorContext(t)); err != nil {
			t.Error(err)
		}
	})
	return owner
}

func assertDirectoryEmpty(t *testing.T, path string) {
	t.Helper()
	files, err := os.ReadDir(path)
	if err != nil || len(files) != 0 {
		t.Fatalf("original resource directory is not empty: entries=%d, %v", len(files), err)
	}
}

func TestAuthoredConverterImportsFilesAndReturnsAnOwnedResource(t *testing.T) {
	workbench, config, output, reference, ctx := authorWorkbench(t)
	prepared, err := workbench.Prepare(ctx)
	if err != nil {
		t.Fatal(err)
	}
	view, err := prepared.Acquire(ctx, authorOwner(t), reference)
	if err != nil {
		t.Fatal(err)
	}
	report := view.Report()
	if report.Converted.Source != reference || !strings.Contains(report.Converted.HTML, "HELLO &lt;WORLD&gt;") || len(report.Diagnostics) != 1 || report.Diagnostics[0].Rule != "title" {
		t.Fatalf("authored converter/validators did not run: %+v", report)
	}
	binding := view.Binding()
	if len(binding.Roots) != 3 || len(binding.Units) != 3 {
		t.Fatalf("one capture did not retain the converter, two validators and file import: %+v", binding)
	}
	if err := workbench.Host.Uninstall(ctx, Installation); !errors.Is(err, frame.ErrInUse) {
		t.Fatalf("live resource did not retain its providers: %v", err)
	}
	if err := view.WithReader(ctx, func(_ context.Context, reader io.Reader) error {
		data, err := io.ReadAll(reader)
		if err == nil && string(data) != report.Converted.HTML {
			return errors.New("resource content differs from conversion")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := view.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := view.WithReader(ctx, func(context.Context, io.Reader) error { return nil }); !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("resource escaped the closed Use: %v", err)
	}
	assertDirectoryEmpty(t, output)
	if err := workbench.Host.Uninstall(ctx, Installation); err != nil {
		t.Fatalf("closed resource still blocked installation retirement: %v", err)
	}
	if err := workbench.Host.WaitRetired(ctx, Installation); err != nil {
		t.Fatal(err)
	}
	assertDirectoryEmpty(t, config.WorkDirectory)
	t.Log("authored FileReader -> Converter import, two validators, one multi-Point capture, later resource read and original release passed")
}

func TestAuthoredResourceCancellationJoinsTheOriginalReader(t *testing.T) {
	workbench, _, output, reference, ctx := authorWorkbench(t)
	prepared, err := workbench.Prepare(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner := authorOwner(t)
	view, err := prepared.Acquire(ctx, owner, reference)
	if err != nil {
		t.Fatal(err)
	}
	entered, cancelled, finish := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var finishOnce sync.Once
	releaseReader := func() { finishOnce.Do(func() { close(finish) }) }
	defer releaseReader()
	result := make(chan error, 1)
	go func() {
		result <- view.WithReader(ctx, func(borrowed context.Context, reader io.Reader) error {
			close(entered)
			<-borrowed.Done()
			data, err := io.ReadAll(reader)
			if err != nil || len(data) == 0 {
				return errors.Join(errors.New("resource closed before its reader joined"), err)
			}
			close(cancelled)
			<-finish
			return context.Cause(borrowed)
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cause := errors.New("author cancelled its preview")
	exit := owner.BeginClose(cause)
	select {
	case <-cancelled:
	case err := <-result:
		t.Fatalf("reader ended without original cancellation: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if exit.Outcome().Released {
		t.Fatal("owner released while its original reader was still active")
	}
	if err := view.WithReader(ctx, func(context.Context, io.Reader) error { return nil }); !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("closed admission accepted a new resource reader: %v", err)
	}
	releaseReader()
	if err := <-result; !errors.Is(err, cause) {
		t.Fatalf("original cancellation cause was lost: %v", err)
	}
	if outcome, err := exit.WaitReleased(ctx); err != nil || !outcome.Released {
		t.Fatalf("original reader did not join: %+v, %v", outcome, err)
	}
	assertDirectoryEmpty(t, output)
}

func TestAuthoredMultiPointCaptureRefusesAChangedValidatorSet(t *testing.T) {
	workbench, _, output, reference, ctx := authorWorkbench(t)
	prepared, err := workbench.Prepare(ctx)
	if err != nil {
		t.Fatal(err)
	}
	point := workbench.Contracts.Validators
	addition, err := frame.Define(frame.Definition{Revision: integrity.Hash([]byte("authoring/extra-validator/1")), Units: []frame.Unit{{
		Declaration: frame.UnitDeclaration{ID: "validation", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "extra", Contract: point.Ref()}}},
		Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
			return []frame.ExportValue{frame.Contribute("extra", point, document.ValidatorMetadata{Name: "extra", Order: 30}, document.Validator(func(context.Context, document.Converted) ([]document.Diagnostic, error) { return nil, nil }))}, nil
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workbench.Host.Install(ctx, frame.InstallationConfig{ID: "additional-validator", Plugin: addition, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := workbench.Host.Activate(ctx, frame.ExportRef{InstallationID: "additional-validator", Release: addition.Revision(), UnitID: "validation", Export: "extra", Contract: point.Ref()}); err != nil {
		t.Fatal(err)
	}
	if view, err := prepared.Acquire(ctx, authorOwner(t), reference); !errors.Is(err, frame.ErrStaleBinding) || view != nil {
		t.Fatalf("stale multi-Point decision reached conversion: view=%v, %v", view, err)
	}
	assertDirectoryEmpty(t, output)
}

func TestAuthoredPartialSetupFailureCleansAndCanBeRepaired(t *testing.T) {
	ctx := authorContext(t)
	config, _, _ := authorFiles(t)
	contracts, err := document.NewContracts()
	if err != nil {
		t.Fatal(err)
	}
	host, err := frame.NewHost(ctx, frame.HostConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := host.Close(authorContext(t)); err != nil {
			t.Error(err)
		}
	})
	if err := host.Register(contracts.Files, contracts.Converters, contracts.Validators); err != nil {
		t.Fatal(err)
	}
	plugin, err := Plugin(contracts)
	if err != nil {
		t.Fatal(err)
	}
	broken := config
	broken.Template.Path = "missing-template.html"
	encoded, err := json.Marshal(broken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.Install(ctx, frame.InstallationConfig{ID: Installation, Plugin: plugin, Configuration: encoded, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	root := frame.ExportRef{InstallationID: Installation, Release: plugin.Revision(), UnitID: "converter", Export: "uppercase", Contract: contracts.Converters.Ref()}
	if _, err := host.Activate(ctx, root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing template did not fail partial setup: %v", err)
	}
	for _, observed := range host.Observations() {
		if observed.Source.UnitID == "converter" {
			if err := host.WaitInstanceRetired(ctx, observed.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	assertDirectoryEmpty(t, config.WorkDirectory)
	snapshot, err := host.Snapshot(ctx, contracts.Converters.Ref())
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := frame.Candidates(snapshot, contracts.Converters)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("partially prepared converter became public: %d candidates, %v", len(candidates), err)
	}
	encoded, err = json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Replace(ctx, Installation, plugin, encoded); err != nil {
		t.Fatal(err)
	}
	if err := host.WaitRetired(ctx, Installation); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Activate(ctx, root); err != nil {
		t.Fatalf("corrected original configuration did not repair setup: %v", err)
	}
	t.Log("missing template error was located at the authored setup; original workspace was removed before configuration repair and successful reactivation")
}

func TestAuthoredFileReaderRejectsAChangedSourceRevision(t *testing.T) {
	workbench, config, output, reference, ctx := authorWorkbench(t)
	prepared, err := workbench.Prepare(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.SourceDirectory, reference.Path), []byte("another revision"), 0600); err != nil {
		t.Fatal(err)
	}
	if view, err := prepared.Acquire(ctx, authorOwner(t), reference); !errors.Is(err, document.ErrSourceChanged) || view != nil {
		t.Fatalf("FileReader consumed a different source revision: view=%v, %v", view, err)
	}
	assertDirectoryEmpty(t, output)
	if err := workbench.Host.Uninstall(ctx, Installation); err != nil {
		t.Fatalf("failed original acquisition retained an unused binding: %v", err)
	}
}
