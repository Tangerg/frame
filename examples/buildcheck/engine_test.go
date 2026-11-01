package buildcheck_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/examples/buildcheck"
	"github.com/Tangerg/frame/integrity"
)

func engine(t *testing.T, text string) (*buildcheck.Engine, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	e, err := buildcheck.New(ctx, buildcheck.NewSource(text))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := e.Close(closeCtx); err != nil {
			t.Error(err)
		}
	})
	return e, ctx
}

func install(t *testing.T, ctx context.Context, e *buildcheck.Engine, id string, exports ...frame.ExportValue) {
	t.Helper()
	declarations := make([]frame.ExportDeclaration, 0, len(exports))
	for _, export := range exports {
		declarations = append(declarations, frame.ExportDeclaration{Name: export.Name, Contract: export.Contract})
	}
	plugin, err := frame.Define(frame.Definition{Revision: integrity.Hash([]byte(id)), Units: []frame.Unit{{Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: declarations}, Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) { return exports, nil }}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Host.Install(ctx, frame.InstallationConfig{ID: frame.InstallationID(id), Plugin: plugin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	roots := make([]frame.ExportRef, 0, len(exports))
	for _, export := range exports {
		roots = append(roots, frame.ExportRef{InstallationID: frame.InstallationID(id), Release: plugin.Revision(), UnitID: "main", Export: export.Name, Contract: export.Contract})
	}
	if _, err := e.Host.Activate(ctx, roots...); err != nil {
		t.Fatal(err)
	}
}

func TestConsumerComposesPassesDiagnosticsFormatterAndAround(t *testing.T) {
	e, ctx := engine(t, "  TODO hello  ")
	result, err := e.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "compiled(TODO hello)" {
		t.Fatalf("sequential passes: %q", result.Output)
	}
	if !slices.Equal(result.Trace, []string{"trace:enter", "pass:normalize", "pass:emit", "trace:leave"}) {
		t.Fatalf("frozen around/pass order: %v", result.Trace)
	}
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Analyzer != "todo" {
		t.Fatalf("parallel diagnostics: %+v", result.Diagnostics)
	}
	if result.Patch.Text != "TODO hello\n" || result.Patch.ExpectedRevision != 1 {
		t.Fatalf("exclusive formatter: %+v", result.Patch)
	}
	if _, err := e.Source.Apply(result.Patch); err != nil {
		t.Fatal(err)
	}
}

func TestParallelAnalysisAndRevisionSafeWriteback(t *testing.T) {
	e, ctx := engine(t, "R1")
	started := make(chan string, 2)
	release := make(chan struct{})
	var exports []frame.ExportValue
	for _, name := range []string{"left", "right"} {
		exports = append(exports, frame.Contribute(name, e.Contracts.Analyzers, buildcheck.AnalyzerMetadata{Name: name, Required: true}, buildcheck.Analyzer(func(ctx context.Context, source buildcheck.SourceRevision) ([]buildcheck.Diagnostic, error) {
			started <- name
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-release:
				return []buildcheck.Diagnostic{{Analyzer: name, Message: source.Text, Line: 1}}, nil
			}
		})))
	}
	install(t, ctx, e, "parallel", exports...)
	type outcome struct {
		result buildcheck.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() { result, err := e.Run(ctx); done <- outcome{result, err} }()
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("analyzers did not run concurrently")
		}
	}
	e.Source.Replace("R2")
	close(release)
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	if _, err := e.Source.Apply(result.result.Patch); !errors.Is(err, buildcheck.ErrStaleRevision) {
		t.Fatalf("R1 overwrote R2: %v", err)
	}
	if e.Source.Read().Text != "R2" {
		t.Fatal("source owner lost the newer revision")
	}
}

func TestOptionalFailureIsCollectedAndRequiredFailureJoinsCancellation(t *testing.T) {
	e, ctx := engine(t, "input")
	optionalFailure := errors.New("optional diagnostic unavailable")
	install(t, ctx, e, "optional", frame.Contribute("optional", e.Contracts.Analyzers, buildcheck.AnalyzerMetadata{Name: "optional"}, buildcheck.Analyzer(func(context.Context, buildcheck.SourceRevision) ([]buildcheck.Diagnostic, error) {
		return nil, optionalFailure
	})))
	result, err := e.Run(ctx)
	if err != nil || len(result.Failures) != 1 || !strings.Contains(result.Failures[0], optionalFailure.Error()) {
		t.Fatalf("optional policy: %+v %v", result, err)
	}
	requiredFailure := errors.New("required analyzer failed")
	joined := make(chan struct{})
	install(t, ctx, e, "required", frame.Contribute("required", e.Contracts.Analyzers, buildcheck.AnalyzerMetadata{Name: "required", Required: true}, buildcheck.Analyzer(func(context.Context, buildcheck.SourceRevision) ([]buildcheck.Diagnostic, error) {
		return nil, requiredFailure
	})), frame.Contribute("cancelled", e.Contracts.Analyzers, buildcheck.AnalyzerMetadata{Name: "cancelled"}, buildcheck.Analyzer(func(ctx context.Context, _ buildcheck.SourceRevision) ([]buildcheck.Diagnostic, error) {
		<-ctx.Done()
		close(joined)
		return nil, ctx.Err()
	})))
	if _, err := e.Run(ctx); !errors.Is(err, requiredFailure) {
		t.Fatalf("required policy: %v", err)
	}
	select {
	case <-joined:
	default:
		t.Fatal("Run returned before an analyzer joined")
	}
}

func TestAroundContinuationCannotEscapeOrRunTwice(t *testing.T) {
	e, ctx := engine(t, "input")
	var saved buildcheck.Next
	var savedSource buildcheck.SourceRevision
	install(t, ctx, e, "around-owner", frame.Contribute("guard", e.Contracts.Around, buildcheck.AroundMetadata{Name: "guard", Order: 20}, buildcheck.Around(func(ctx context.Context, source buildcheck.SourceRevision, next buildcheck.Next) (buildcheck.Result, error) {
		saved = next
		savedSource = source
		result, err := next(ctx, source)
		if err != nil {
			return result, err
		}
		if _, err := next(ctx, source); err == nil {
			return result, errors.New("continuation ran twice")
		}
		return result, nil
	})))
	if _, err := e.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if saved == nil {
		t.Fatal("around was not selected")
	}
	if _, err := saved(ctx, savedSource); err == nil {
		t.Fatal("continuation escaped its original call")
	}
}
