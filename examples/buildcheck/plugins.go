package buildcheck

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/internal/cancellation"
	"github.com/Tangerg/frame/value"
)

const (
	normalizePassOrder        = 10
	emitPassOrder             = 20
	trimFormatterPriority     = 10
	identityFormatterPriority = 1
	traceAroundOrder          = 1
	maximumLineBytes          = 80
)

func StandardInstallations(contracts Contracts) ([]frame.InstallationConfig, error) {
	definitions := []struct {
		installation frame.InstallationID
		revision     string
		unit         frame.UnitSpec
	}{
		{"compiler", "buildcheck/compiler/1", compilerUnit(contracts.Passes)},
		{"analysis", "buildcheck/analysis/1", analysisUnit(contracts.Analyzers)},
		{"format", "buildcheck/format/1", formatterUnit(contracts.Formatters)},
		{"trace", "buildcheck/trace/1", tracingUnit(contracts.Around)},
	}
	installations := make([]frame.InstallationConfig, 0, len(definitions))
	for _, definition := range definitions {
		unit, err := frame.DefineUnit(definition.unit)
		if err != nil {
			return nil, err
		}
		plugin, err := frame.Define(frame.Definition{Revision: value.Hash([]byte(definition.revision)), Units: []frame.Unit{unit}})
		if err != nil {
			return nil, err
		}
		installations = append(installations, frame.InstallationConfig{ID: definition.installation, Plugin: plugin, Enabled: true})
	}
	return installations, nil
}

func compilerUnit(point frame.Point[PassMetadata, CompilerPass]) frame.UnitSpec {
	normalization := PassMetadata{Stage: "normalize", Order: normalizePassOrder}
	emission := PassMetadata{Stage: "emit", Order: emitPassOrder}
	normalize := frame.Offer(normalization.Stage, point)
	emit := frame.Offer(emission.Stage, point)
	return frame.UnitSpec{ID: "compiler", Exports: frame.Exports(normalize, emit), Prepare: func(setup *frame.Setup) error {
		if err := normalize.Bind(setup, normalization, CompilerPass(func(ctx context.Context, input string) (string, error) {
			if err := cancellation.Err(ctx); err != nil {
				return "", err
			}
			return strings.TrimSpace(input), nil
		})); err != nil {
			return err
		}
		return emit.Bind(setup, emission, CompilerPass(func(ctx context.Context, input string) (string, error) {
			if err := cancellation.Err(ctx); err != nil {
				return "", err
			}
			return "compiled(" + input + ")", nil
		}))
	}}
}

type lineAnalyzer struct {
	metadata   AnalyzerMetadata
	message    string
	violatedBy func(string) bool
}

func (a lineAnalyzer) analyze(ctx context.Context, input SourceRevision) ([]Diagnostic, error) {
	var found []Diagnostic
	for line, text := range strings.Split(input.Text, "\n") {
		if err := cancellation.Err(ctx); err != nil {
			return nil, err
		}
		if a.violatedBy(text) {
			found = append(found, Diagnostic{Analyzer: a.metadata.Name, Line: line + 1, Message: a.message})
		}
	}
	return found, nil
}

func analysisUnit(point frame.Point[AnalyzerMetadata, Analyzer]) frame.UnitSpec {
	unfinished := lineAnalyzer{
		metadata:   AnalyzerMetadata{Name: "todo", Required: true},
		message:    "unfinished work",
		violatedBy: func(text string) bool { return strings.Contains(text, "TODO") },
	}
	longLine := lineAnalyzer{
		metadata:   AnalyzerMetadata{Name: "long-line"},
		message:    fmt.Sprintf("line exceeds %d bytes", maximumLineBytes),
		violatedBy: func(text string) bool { return len(text) > maximumLineBytes },
	}
	todo := frame.Offer(unfinished.metadata.Name, point)
	length := frame.Offer(longLine.metadata.Name, point)
	return frame.UnitSpec{ID: "analysis", Exports: frame.Exports(todo, length), Prepare: func(setup *frame.Setup) error {
		if err := todo.Bind(setup, unfinished.metadata, Analyzer(unfinished.analyze)); err != nil {
			return err
		}
		return length.Bind(setup, longLine.metadata, Analyzer(longLine.analyze))
	}}
}

func formatterUnit(point frame.Point[FormatterMetadata, Formatter]) frame.UnitSpec {
	trimmed := FormatterMetadata{Name: "trim", Priority: trimFormatterPriority}
	unchanged := FormatterMetadata{Name: "identity", Priority: identityFormatterPriority}
	trim := frame.Offer(trimmed.Name, point)
	identity := frame.Offer(unchanged.Name, point)
	return frame.UnitSpec{ID: "format", Exports: frame.Exports(trim, identity), Prepare: func(setup *frame.Setup) error {
		if err := trim.Bind(setup, trimmed, Formatter(func(ctx context.Context, input SourceRevision) (Patch, error) {
			if err := cancellation.Err(ctx); err != nil {
				return Patch{}, err
			}
			return Patch{ExpectedRevision: input.Revision, Text: strings.TrimSpace(input.Text) + "\n"}, nil
		})); err != nil {
			return err
		}
		return identity.Bind(setup, unchanged, Formatter(func(ctx context.Context, input SourceRevision) (Patch, error) {
			if err := cancellation.Err(ctx); err != nil {
				return Patch{}, err
			}
			return Patch{ExpectedRevision: input.Revision, Text: input.Text}, nil
		}))
	}}
}

func tracingUnit(point frame.Point[AroundMetadata, Around]) frame.UnitSpec {
	metadata := AroundMetadata{Name: "trace", Order: traceAroundOrder}
	trace := frame.Offer(metadata.Name, point)
	return frame.UnitSpec{ID: "trace", Exports: frame.Exports(trace), Prepare: func(setup *frame.Setup) error {
		return trace.Bind(setup, metadata, Around(func(ctx context.Context, input SourceRevision, next Next) (Result, error) {
			if next == nil {
				return Result{}, errors.New("buildcheck: continuation is required")
			}
			result, err := next(ctx, input)
			result.Trace = append([]string{metadata.Name + ":enter"}, result.Trace...)
			result.Trace = append(result.Trace, metadata.Name+":leave")
			return result, err
		}))
	}}
}
