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
	installations, err := StandardInstallations(contracts)
	if err != nil {
		return nil, err
	}
	var roots []frame.ExportRef
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
	passSelection, err := frame.SelectMany(snapshot, e.Contracts.Passes, "build.passes/1", orderCompilerPasses)
	if err != nil {
		return Result{}, err
	}
	analyzerSelection, err := frame.SelectMany(snapshot, e.Contracts.Analyzers, "build.analyzers/1", orderAnalyzers)
	if err != nil {
		return Result{}, err
	}
	formatterSelection, err := frame.SelectOne(snapshot, e.Contracts.Formatters, "build.formatter/1", selectFormatter)
	if err != nil {
		return Result{}, err
	}
	aroundSelection, err := frame.SelectMany(snapshot, e.Contracts.Around, "build.around/1", orderWrappers)
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

func orderCompilerPasses(values []frame.Candidate[PassMetadata, CompilerPass]) ([]frame.Candidate[PassMetadata, CompilerPass], error) {
	type passCandidate struct {
		candidate frame.Candidate[PassMetadata, CompilerPass]
		metadata  PassMetadata
	}
	passes := make([]passCandidate, len(values))
	for index, candidate := range values {
		metadata, err := candidate.Metadata()
		if err != nil {
			return nil, err
		}
		passes[index] = passCandidate{candidate, metadata}
	}
	slices.SortFunc(passes, func(a, b passCandidate) int { return cmp.Compare(a.metadata.Order, b.metadata.Order) })
	for index, pass := range passes {
		values[index] = pass.candidate
	}
	return values, nil
}

func orderAnalyzers(values []frame.Candidate[AnalyzerMetadata, Analyzer]) ([]frame.Candidate[AnalyzerMetadata, Analyzer], error) {
	type analyzerCandidate struct {
		candidate frame.Candidate[AnalyzerMetadata, Analyzer]
		metadata  AnalyzerMetadata
	}
	analyzers := make([]analyzerCandidate, len(values))
	for index, candidate := range values {
		metadata, err := candidate.Metadata()
		if err != nil {
			return nil, err
		}
		analyzers[index] = analyzerCandidate{candidate, metadata}
	}
	slices.SortFunc(analyzers, func(a, b analyzerCandidate) int { return cmp.Compare(a.metadata.Name, b.metadata.Name) })
	for index, analyzer := range analyzers {
		values[index] = analyzer.candidate
	}
	return values, nil
}

func selectFormatter(values []frame.Candidate[FormatterMetadata, Formatter]) (frame.Candidate[FormatterMetadata, Formatter], error) {
	type formatterCandidate struct {
		candidate frame.Candidate[FormatterMetadata, Formatter]
		metadata  FormatterMetadata
	}
	formatters := make([]formatterCandidate, len(values))
	for index, candidate := range values {
		metadata, err := candidate.Metadata()
		if err != nil {
			return frame.Candidate[FormatterMetadata, Formatter]{}, err
		}
		formatters[index] = formatterCandidate{candidate, metadata}
	}
	if len(formatters) == 0 {
		return frame.Candidate[FormatterMetadata, Formatter]{}, errors.New("buildcheck: no formatter is available")
	}
	selected := slices.MinFunc(formatters, func(a, b formatterCandidate) int {
		return cmp.Or(cmp.Compare(b.metadata.Priority, a.metadata.Priority), cmp.Compare(a.metadata.Name, b.metadata.Name))
	})
	return selected.candidate, nil
}

func orderWrappers(values []frame.Candidate[AroundMetadata, Around]) ([]frame.Candidate[AroundMetadata, Around], error) {
	type wrapperCandidate struct {
		candidate frame.Candidate[AroundMetadata, Around]
		metadata  AroundMetadata
	}
	wrappers := make([]wrapperCandidate, len(values))
	for index, candidate := range values {
		metadata, err := candidate.Metadata()
		if err != nil {
			return nil, err
		}
		wrappers[index] = wrapperCandidate{candidate, metadata}
	}
	slices.SortFunc(wrappers, func(a, b wrapperCandidate) int {
		return cmp.Or(cmp.Compare(a.metadata.Order, b.metadata.Order), cmp.Compare(a.metadata.Name, b.metadata.Name))
	})
	for index, wrapper := range wrappers {
		values[index] = wrapper.candidate
	}
	return values, nil
}

func (e *Engine) Close(ctx context.Context) error { _, err := e.Host.Close(ctx); return err }
