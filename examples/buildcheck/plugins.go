package buildcheck

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
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

func StandardInstallations(contracts Contracts) ([]framehost.InstallationConfig, error) {
	definitions := []struct {
		installation framehost.InstallationID
		revision     string
		unit         framehost.UnitSpec
	}{
		{"compiler", "buildcheck/compiler/1", compilerUnit(contracts.Passes)},
		{"analysis", "buildcheck/analysis/1", analysisUnit(contracts.Analyzers)},
		{"format", "buildcheck/format/1", formatterUnit(contracts.Formatters)},
		{"trace", "buildcheck/trace/1", tracingUnit(contracts.Around)},
	}
	installations := make([]framehost.InstallationConfig, 0, len(definitions))
	for _, definition := range definitions {
		unit, err := framehost.DefineUnit(definition.unit)
		if err != nil {
			return nil, err
		}
		plugin, err := framehost.Define(framehost.Definition{Revision: value.Hash([]byte(definition.revision)), Units: []framehost.Unit{unit}})
		if err != nil {
			return nil, err
		}
		installations = append(installations, framehost.InstallationConfig{ID: definition.installation, Plugin: plugin, Enabled: true})
	}
	return installations, nil
}

func compilerUnit(point framehost.Point[PassMetadata, CompilerPass]) framehost.UnitSpec {
	normalization := PassMetadata{Stage: "normalize", Order: normalizePassOrder}
	emission := PassMetadata{Stage: "emit", Order: emitPassOrder}
	normalize := framehost.Offer(normalization.Stage, point)
	emit := framehost.Offer(emission.Stage, point)
	return framehost.UnitSpec{ID: "compiler", Exports: framehost.Exports(normalize, emit), Prepare: func(setup *framehost.Setup) error {
		if err := normalize.Bind(setup, normalization, CompilerPass(func(ctx context.Context, input string) (string, error) {
			if err := frame.ContextError(ctx); err != nil {
				return "", err
			}
			return strings.TrimSpace(input), nil
		})); err != nil {
			return err
		}
		return emit.Bind(setup, emission, CompilerPass(func(ctx context.Context, input string) (string, error) {
			if err := frame.ContextError(ctx); err != nil {
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
		if err := frame.ContextError(ctx); err != nil {
			return nil, err
		}
		if a.violatedBy(text) {
			found = append(found, Diagnostic{Analyzer: a.metadata.Name, Line: line + 1, Message: a.message})
		}
	}
	return found, nil
}

func analysisUnit(point framehost.Point[AnalyzerMetadata, Analyzer]) framehost.UnitSpec {
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
	todo := framehost.Offer(unfinished.metadata.Name, point)
	length := framehost.Offer(longLine.metadata.Name, point)
	return framehost.UnitSpec{ID: "analysis", Exports: framehost.Exports(todo, length), Prepare: func(setup *framehost.Setup) error {
		if err := todo.Bind(setup, unfinished.metadata, Analyzer(unfinished.analyze)); err != nil {
			return err
		}
		return length.Bind(setup, longLine.metadata, Analyzer(longLine.analyze))
	}}
}

func formatterUnit(point framehost.Point[FormatterMetadata, Formatter]) framehost.UnitSpec {
	trimmed := FormatterMetadata{Name: "trim", Priority: trimFormatterPriority}
	unchanged := FormatterMetadata{Name: "identity", Priority: identityFormatterPriority}
	trim := framehost.Offer(trimmed.Name, point)
	identity := framehost.Offer(unchanged.Name, point)
	return framehost.UnitSpec{ID: "format", Exports: framehost.Exports(trim, identity), Prepare: func(setup *framehost.Setup) error {
		if err := trim.Bind(setup, trimmed, Formatter(func(ctx context.Context, input SourceRevision) (Patch, error) {
			if err := frame.ContextError(ctx); err != nil {
				return Patch{}, err
			}
			return Patch{ExpectedRevision: input.Revision, Text: strings.TrimSpace(input.Text) + "\n"}, nil
		})); err != nil {
			return err
		}
		return identity.Bind(setup, unchanged, Formatter(func(ctx context.Context, input SourceRevision) (Patch, error) {
			if err := frame.ContextError(ctx); err != nil {
				return Patch{}, err
			}
			return Patch{ExpectedRevision: input.Revision, Text: input.Text}, nil
		}))
	}}
}

func tracingUnit(point framehost.Point[AroundMetadata, Around]) framehost.UnitSpec {
	metadata := AroundMetadata{Name: "trace", Order: traceAroundOrder}
	trace := framehost.Offer(metadata.Name, point)
	return framehost.UnitSpec{ID: "trace", Exports: framehost.Exports(trace), Prepare: func(setup *framehost.Setup) error {
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
