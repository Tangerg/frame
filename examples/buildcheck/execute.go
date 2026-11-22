package buildcheck

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
)

func execute(ctx context.Context, source SourceRevision, passes []framehost.Member[PassMetadata, CompilerPass], analyzers []framehost.Member[AnalyzerMetadata, Analyzer], formatter Formatter, around []framehost.Member[AroundMetadata, Around]) (Result, error) {
	run := check(passes, analyzers, formatter)
	for i := len(around) - 1; i >= 0; i-- {
		run = wrap(around[i].Value, run)
	}
	return run(ctx, source)
}

func check(passes []framehost.Member[PassMetadata, CompilerPass], analyzers []framehost.Member[AnalyzerMetadata, Analyzer], formatter Formatter) Next {
	return func(ctx context.Context, input SourceRevision) (Result, error) {
		result := Result{Source: input, Output: input.Text, Diagnostics: []Diagnostic{}, Failures: []string{}, Trace: []string{}}
		for _, pass := range passes {
			if err := frame.ContextError(ctx); err != nil {
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
	}
}

func wrap(wrapper Around, next Next) Next {
	return func(ctx context.Context, input SourceRevision) (Result, error) {
		inner, cancel := context.WithCancelCause(ctx)
		defer cancel(nil)
		gate := &continuation{open: true, input: input, next: next, owner: inner}
		result, err := gate.around(wrapper, cancel)
		if !gate.continued() && err == nil {
			return result, errors.New("buildcheck: around contract requires one continuation")
		}
		if result.Source.Revision != 0 && result.Source != input {
			return result, ErrStaleRevision
		}
		return result, err
	}
}

// continuation is the single call an around wrapper may make to the next
// stage while its own call is open. The wrapper's return joins that call, so a
// continuation can never outlive or run after the wrapper that admitted it.
type continuation struct {
	mu      sync.Mutex
	open    bool
	called  bool
	running sync.WaitGroup
	input   SourceRevision
	next    Next
	owner   context.Context
}

func (c *continuation) around(wrapper Around, cancel context.CancelCauseFunc) (result Result, err error) {
	returned := false
	defer func() {
		c.mu.Lock()
		c.open = false
		c.mu.Unlock()
		if !returned || err != nil {
			cancel(err)
		}
		c.running.Wait()
	}()
	result, err = wrapper(c.owner, c.input, c.call)
	returned = true
	return result, err
}

func (c *continuation) enter(input SourceRevision) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.open || c.called {
		return errors.New("buildcheck: continuation is once-only within its original call")
	}
	if input != c.input {
		return ErrStaleRevision
	}
	c.called = true
	c.running.Add(1)
	return nil
}

func (c *continuation) call(ctx context.Context, input SourceRevision) (Result, error) {
	if err := c.enter(input); err != nil {
		return Result{}, err
	}
	defer c.running.Done()
	owned, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(c.owner, func() { cancel(frame.ContextError(c.owner)) })
	defer stop()
	defer cancel(nil)
	if err := frame.ContextError(c.owner); err != nil {
		return Result{}, err
	}
	if err := frame.ContextError(owned); err != nil {
		return Result{}, err
	}
	return c.next(owned, input)
}

func (c *continuation) continued() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.called
}

func analyze(ctx context.Context, source SourceRevision, analyzers []framehost.Member[AnalyzerMetadata, Analyzer]) ([]Diagnostic, []string, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	type outcome struct {
		diagnostics []Diagnostic
		err         error
	}
	results := make([]outcome, len(analyzers))
	var joined sync.WaitGroup
	for i, analyzer := range analyzers {
		results[i].err = errors.New("buildcheck: analyzer exited without returning")
		joined.Add(1)
		go func() {
			defer func() {
				if analyzer.Metadata.Required && results[i].err != nil {
					cancel(results[i].err)
				}
				joined.Done()
			}()
			results[i].diagnostics, results[i].err = analyzer.Value(ctx, source)
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
			return cmp.Compare(a.Line, b.Line)
		}
		return strings.Compare(a.Analyzer, b.Analyzer)
	})
	return diagnostics, failures, errors.Join(append(required, frame.ContextError(ctx))...)
}

func definitionExports(id framehost.InstallationID, plugin *framehost.Plugin) []framehost.ExportRef {
	var exports []framehost.ExportRef
	for _, unit := range plugin.Units() {
		for _, export := range unit.Exports {
			exports = append(exports, framehost.ExportRef{InstallationID: id, Release: plugin.Revision(), UnitID: unit.ID, Export: export.Name, Contract: export.Contract})
		}
	}
	return exports
}
