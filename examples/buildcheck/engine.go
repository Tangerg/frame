package buildcheck

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	framehost "github.com/Tangerg/frame/host"
)

type Engine struct {
	Host      framehost.Host
	Contracts Contracts
	Source    *Source
	scope     framehost.ScopeGrant
}

func New(ctx context.Context, source *Source) (*Engine, error) {
	if source == nil {
		return nil, errors.New("buildcheck: source is required")
	}
	contracts, err := NewContracts()
	if err != nil {
		return nil, err
	}
	host, err := framehost.New(ctx, framehost.Config{})
	if err != nil {
		return nil, err
	}
	engine := &Engine{Host: host, Contracts: contracts, Source: source}
	if err := host.Register(contracts.Passes, contracts.Analyzers, contracts.Formatters, contracts.Around); err != nil {
		return nil, err
	}
	installations, err := StandardInstallations(contracts)
	if err != nil {
		return nil, err
	}
	var roots []framehost.ExportRef
	for _, installation := range installations {
		if _, err := host.Install(ctx, installation); err != nil {
			return nil, err
		}
		roots = append(roots, definitionExports(installation.ID, installation.Plugin)...)
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
	passSelection, err := framehost.SelectMany(snapshot, e.Contracts.Passes, "build.passes/1", orderCompilerPasses)
	if err != nil {
		return Result{}, err
	}
	analyzerSelection, err := framehost.SelectMany(snapshot, e.Contracts.Analyzers, "build.analyzers/1", orderAnalyzers)
	if err != nil {
		return Result{}, err
	}
	formatterSelection, err := framehost.SelectOne(snapshot, e.Contracts.Formatters, "build.formatter/1", selectFormatter)
	if err != nil {
		return Result{}, err
	}
	aroundSelection, err := framehost.SelectMany(snapshot, e.Contracts.Around, "build.around/1", orderWrappers)
	if err != nil {
		return Result{}, err
	}
	plan, err := framehost.Capture(snapshot, passSelection.Ref(), analyzerSelection.Ref(), formatterSelection.Ref(), aroundSelection.Ref())
	if err != nil {
		return Result{}, err
	}
	var result Result
	err = e.Host.With(ctx, e.scope, plan, func(access framehost.Access) error {
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

// Each rule orders candidates by their metadata; equal metadata keeps snapshot order.
func orderCompilerPasses(candidates []framehost.Candidate[PassMetadata, CompilerPass]) ([]framehost.Candidate[PassMetadata, CompilerPass], error) {
	slices.SortStableFunc(candidates, func(a, b framehost.Candidate[PassMetadata, CompilerPass]) int {
		return cmp.Compare(a.Metadata.Order, b.Metadata.Order)
	})
	return candidates, nil
}

func orderAnalyzers(candidates []framehost.Candidate[AnalyzerMetadata, Analyzer]) ([]framehost.Candidate[AnalyzerMetadata, Analyzer], error) {
	slices.SortStableFunc(candidates, func(a, b framehost.Candidate[AnalyzerMetadata, Analyzer]) int {
		return cmp.Compare(a.Metadata.Name, b.Metadata.Name)
	})
	return candidates, nil
}

func selectFormatter(candidates []framehost.Candidate[FormatterMetadata, Formatter]) (framehost.Candidate[FormatterMetadata, Formatter], error) {
	if len(candidates) == 0 {
		return framehost.Candidate[FormatterMetadata, Formatter]{}, fmt.Errorf("%w: no formatter", framehost.ErrUnavailable)
	}
	return slices.MinFunc(candidates, func(a, b framehost.Candidate[FormatterMetadata, Formatter]) int {
		return cmp.Or(cmp.Compare(b.Metadata.Priority, a.Metadata.Priority), cmp.Compare(a.Metadata.Name, b.Metadata.Name))
	}), nil
}

func orderWrappers(candidates []framehost.Candidate[AroundMetadata, Around]) ([]framehost.Candidate[AroundMetadata, Around], error) {
	slices.SortStableFunc(candidates, func(a, b framehost.Candidate[AroundMetadata, Around]) int {
		return cmp.Or(cmp.Compare(a.Metadata.Order, b.Metadata.Order), cmp.Compare(a.Metadata.Name, b.Metadata.Name))
	})
	return candidates, nil
}

func (e *Engine) Close(ctx context.Context) error { _, err := e.Host.Close(ctx); return err }
