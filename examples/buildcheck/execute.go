package buildcheck

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/Tangerg/frame"
)

func execute(ctx context.Context, source SourceRevision, passes []frame.Member[PassMetadata, CompilerPass], analyzers []frame.Member[AnalyzerMetadata, Analyzer], formatter Formatter, around []frame.Member[AroundMetadata, Around]) (Result, error) {
	base := Next(func(ctx context.Context, input SourceRevision) (Result, error) {
		result := Result{Source: input, Output: input.Text, Diagnostics: []Diagnostic{}, Failures: []string{}, Trace: []string{}}
		for _, pass := range passes {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			output, err := pass.Value(ctx, result.Output)
			if err != nil {
				return result, fmt.Errorf("buildcheck: %s pass: %w", pass.Metadata.Stage, err)
			}
			result.Output = output
			result.Trace = append(result.Trace, "pass:"+pass.Metadata.Stage)
		}
		diagnostics, failures, err := analyze(ctx, input, analyzers)
		result.Diagnostics = diagnostics
		result.Failures = failures
		if err != nil {
			return result, err
		}
		result.Patch, err = formatter(ctx, input)
		if err != nil {
			return result, err
		}
		if result.Patch.ExpectedRevision != input.Revision {
			return result, ErrStaleRevision
		}
		return result, nil
	})
	for i := len(around) - 1; i >= 0; i-- {
		wrapper, next := around[i], base
		base = func(ctx context.Context, input SourceRevision) (Result, error) {
			var mu sync.Mutex
			open, called := true, false
			var running sync.WaitGroup
			continuation := Next(func(nextCtx context.Context, nextInput SourceRevision) (Result, error) {
				mu.Lock()
				if !open || called {
					mu.Unlock()
					return Result{}, errors.New("buildcheck: continuation is once-only within its original call")
				}
				if nextInput != input {
					mu.Unlock()
					return Result{}, ErrStaleRevision
				}
				called = true
				running.Add(1)
				mu.Unlock()
				defer running.Done()
				owned, cancel := context.WithCancelCause(nextCtx)
				stop := context.AfterFunc(ctx, func() { cancel(context.Cause(ctx)) })
				defer stop()
				defer cancel(nil)
				if err := ctx.Err(); err != nil {
					return Result{}, err
				}
				return next(owned, nextInput)
			})
			result, err := wrapper.Value(ctx, input, continuation)
			mu.Lock()
			open = false
			mu.Unlock()
			running.Wait()
			if !called && err == nil {
				return result, errors.New("buildcheck: around contract requires one continuation")
			}
			if result.Source.Revision != 0 && result.Source != input {
				return result, ErrStaleRevision
			}
			return result, err
		}
	}
	return base(ctx, source)
}

func analyze(ctx context.Context, source SourceRevision, analyzers []frame.Member[AnalyzerMetadata, Analyzer]) ([]Diagnostic, []string, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	type outcome struct {
		diagnostics []Diagnostic
		err         error
	}
	results := make([]outcome, len(analyzers))
	var joined sync.WaitGroup
	for i, analyzer := range analyzers {
		joined.Add(1)
		go func() {
			defer joined.Done()
			results[i].diagnostics, results[i].err = analyzer.Value(ctx, source)
			if analyzer.Metadata.Required && results[i].err != nil {
				cancel(results[i].err)
			}
		}()
	}
	joined.Wait()
	var diagnostics []Diagnostic
	var failures []string
	var required []error
	for i, result := range results {
		if result.err != nil {
			if analyzers[i].Metadata.Required {
				required = append(required, fmt.Errorf("buildcheck: analyzer %s: %w", analyzers[i].Metadata.Name, result.err))
			} else {
				failures = append(failures, analyzers[i].Metadata.Name+": "+result.err.Error())
			}
			continue
		}
		diagnostics = append(diagnostics, result.diagnostics...)
	}
	slices.SortFunc(diagnostics, func(a, b Diagnostic) int {
		if a.Line != b.Line {
			return a.Line - b.Line
		}
		return strings.Compare(a.Analyzer, b.Analyzer)
	})
	return diagnostics, failures, errors.Join(append(required, ctx.Err())...)
}

func definitionExports(id frame.InstallationID, plugin *frame.Plugin) []frame.ExportRef {
	var exports []frame.ExportRef
	for _, unit := range plugin.Units() {
		for _, export := range unit.Exports {
			exports = append(exports, frame.ExportRef{InstallationID: id, Release: plugin.Revision(), UnitID: unit.ID, Export: export.Name, Contract: export.Contract})
		}
	}
	return exports
}
