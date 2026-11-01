package buildcheck

import (
	"cmp"
	"context"
	"errors"
	"slices"

	"github.com/Tangerg/frame"
)

type Engine struct {
	Host      *frame.Host
	Contracts Contracts
	Source    *Source
	scope     frame.ScopeGrant
}

func New(ctx context.Context, source *Source) (*Engine, error) {
	if source == nil {
		return nil, errors.New("buildcheck: source is required")
	}
	contracts, err := NewContracts()
	if err != nil {
		return nil, err
	}
	host, err := frame.NewHost(ctx, frame.HostConfig{})
	if err != nil {
		return nil, err
	}
	engine := &Engine{Host: host, Contracts: contracts, Source: source}
	if err := host.Register(contracts.Passes, contracts.Analyzers, contracts.Formatters, contracts.Around); err != nil {
		return nil, err
	}
	plugins, err := StandardPlugins(contracts)
	if err != nil {
		return nil, err
	}
	ids := []frame.InstallationID{"compiler", "analysis", "format", "trace"}
	var roots []frame.ExportRef
	for i, plugin := range plugins {
		if _, err := host.Install(ctx, frame.InstallationConfig{ID: ids[i], Plugin: plugin, Enabled: true}); err != nil {
			return nil, err
		}
		roots = append(roots, definitionExports(ids[i], plugin)...)
	}
	if _, err := host.Activate(ctx, roots...); err != nil {
		return nil, err
	}
	engine.scope, err = host.Admission().OpenScope(ctx, "build:source")
	if err != nil {
		return nil, err
	}
	return engine, nil
}

// Run reads the source once, then captures all four Points together. The
// framework protects implementations; Source remains the only writeback owner.
func (e *Engine) Run(ctx context.Context) (Result, error) {
	source := e.Source.Read()
	snapshot, err := e.Host.Snapshot(ctx, e.Contracts.Passes.Ref(), e.Contracts.Analyzers.Ref(), e.Contracts.Formatters.Ref(), e.Contracts.Around.Ref())
	if err != nil {
		return Result{}, err
	}
	passes, err := frame.Candidates(snapshot, e.Contracts.Passes)
	if err != nil {
		return Result{}, err
	}
	slices.SortFunc(passes, func(a, b frame.Candidate[PassMetadata, CompilerPass]) int {
		return cmp.Compare(a.Metadata().Order, b.Metadata().Order)
	})
	analyzers, err := frame.Candidates(snapshot, e.Contracts.Analyzers)
	if err != nil {
		return Result{}, err
	}
	slices.SortFunc(analyzers, func(a, b frame.Candidate[AnalyzerMetadata, Analyzer]) int {
		return cmp.Compare(a.Metadata().Name, b.Metadata().Name)
	})
	formatters, err := frame.Candidates(snapshot, e.Contracts.Formatters)
	if err != nil {
		return Result{}, err
	}
	slices.SortFunc(formatters, func(a, b frame.Candidate[FormatterMetadata, Formatter]) int {
		return cmp.Or(cmp.Compare(b.Metadata().Priority, a.Metadata().Priority), cmp.Compare(a.Metadata().Name, b.Metadata().Name))
	})
	if len(formatters) == 0 {
		return Result{}, errors.New("buildcheck: no formatter is available")
	}
	around, err := frame.Candidates(snapshot, e.Contracts.Around)
	if err != nil {
		return Result{}, err
	}
	slices.SortFunc(around, func(a, b frame.Candidate[AroundMetadata, Around]) int {
		return cmp.Or(cmp.Compare(a.Metadata().Order, b.Metadata().Order), cmp.Compare(a.Metadata().Name, b.Metadata().Name))
	})
	passSelection, err := frame.Select(snapshot, e.Contracts.Passes, "build.passes/1", passes...)
	if err != nil {
		return Result{}, err
	}
	analyzerSelection, err := frame.Select(snapshot, e.Contracts.Analyzers, "build.analyzers/1", analyzers...)
	if err != nil {
		return Result{}, err
	}
	formatterSelection, err := frame.Select(snapshot, e.Contracts.Formatters, "build.formatter/1", formatters[0])
	if err != nil {
		return Result{}, err
	}
	aroundSelection, err := frame.Select(snapshot, e.Contracts.Around, "build.around/1", around...)
	if err != nil {
		return Result{}, err
	}
	plan, err := frame.Capture(snapshot, passSelection.Ref(), analyzerSelection.Ref(), formatterSelection.Ref(), aroundSelection.Ref())
	if err != nil {
		return Result{}, err
	}
	var result Result
	err = e.Host.With(ctx, e.scope, plan, func(access *frame.Access) error {
		orderedPasses, err := passSelection.Members(access)
		if err != nil {
			return err
		}
		parallelAnalyzers, err := analyzerSelection.Members(access)
		if err != nil {
			return err
		}
		formatter, err := formatterSelection.One(access)
		if err != nil {
			return err
		}
		wrappers, err := aroundSelection.Members(access)
		if err != nil {
			return err
		}
		result, err = execute(access.Context(), source, orderedPasses, parallelAnalyzers, formatter.Value, wrappers)
		return err
	})
	return result, err
}

func (e *Engine) Close(ctx context.Context) error { _, err := e.Host.Close(ctx); return err }
