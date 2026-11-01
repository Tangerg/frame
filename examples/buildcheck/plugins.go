package buildcheck

import (
	"context"
	"errors"
	"strings"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

func StandardPlugins(contracts Contracts) ([]*frame.Plugin, error) {
	definitions := []frame.Definition{
		{Revision: value.Hash([]byte("buildcheck/compiler/1")), Units: []frame.Unit{{Declaration: frame.UnitDeclaration{ID: "compiler", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "normalize", Contract: contracts.Passes.Ref()}, {Name: "emit", Contract: contracts.Passes.Ref()}}}, Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
			return []frame.ExportValue{
				frame.Contribute("normalize", contracts.Passes, PassMetadata{Stage: "normalize", Order: 10}, CompilerPass(func(ctx context.Context, input string) (string, error) {
					if err := ctx.Err(); err != nil {
						return "", err
					}
					return strings.TrimSpace(input), nil
				})),
				frame.Contribute("emit", contracts.Passes, PassMetadata{Stage: "emit", Order: 20}, CompilerPass(func(ctx context.Context, input string) (string, error) {
					if err := ctx.Err(); err != nil {
						return "", err
					}
					return "compiled(" + input + ")", nil
				})),
			}, nil
		}}}},
		{Revision: value.Hash([]byte("buildcheck/analysis/1")), Units: []frame.Unit{{Declaration: frame.UnitDeclaration{ID: "analysis", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "todo", Contract: contracts.Analyzers.Ref()}, {Name: "long-line", Contract: contracts.Analyzers.Ref()}}}, Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
			return []frame.ExportValue{
				frame.Contribute("todo", contracts.Analyzers, AnalyzerMetadata{Name: "todo", Required: true}, Analyzer(func(ctx context.Context, input SourceRevision) ([]Diagnostic, error) {
					var found []Diagnostic
					for line, text := range strings.Split(input.Text, "\n") {
						if err := ctx.Err(); err != nil {
							return nil, err
						}
						if strings.Contains(text, "TODO") {
							found = append(found, Diagnostic{Analyzer: "todo", Line: line + 1, Message: "unfinished work"})
						}
					}
					return found, nil
				})),
				frame.Contribute("long-line", contracts.Analyzers, AnalyzerMetadata{Name: "long-line"}, Analyzer(func(ctx context.Context, input SourceRevision) ([]Diagnostic, error) {
					var found []Diagnostic
					for line, text := range strings.Split(input.Text, "\n") {
						if err := ctx.Err(); err != nil {
							return nil, err
						}
						if len(text) > 80 {
							found = append(found, Diagnostic{Analyzer: "long-line", Line: line + 1, Message: "line exceeds 80 bytes"})
						}
					}
					return found, nil
				})),
			}, nil
		}}}},
		{Revision: value.Hash([]byte("buildcheck/format/1")), Units: []frame.Unit{{Declaration: frame.UnitDeclaration{ID: "format", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "trim", Contract: contracts.Formatters.Ref()}, {Name: "identity", Contract: contracts.Formatters.Ref()}}}, Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
			return []frame.ExportValue{
				frame.Contribute("trim", contracts.Formatters, FormatterMetadata{Name: "trim", Priority: 10}, Formatter(func(ctx context.Context, input SourceRevision) (Patch, error) {
					if err := ctx.Err(); err != nil {
						return Patch{}, err
					}
					return Patch{ExpectedRevision: input.Revision, Text: strings.TrimSpace(input.Text) + "\n"}, nil
				})),
				frame.Contribute("identity", contracts.Formatters, FormatterMetadata{Name: "identity", Priority: 1}, Formatter(func(ctx context.Context, input SourceRevision) (Patch, error) {
					if err := ctx.Err(); err != nil {
						return Patch{}, err
					}
					return Patch{ExpectedRevision: input.Revision, Text: input.Text}, nil
				})),
			}, nil
		}}}},
		{Revision: value.Hash([]byte("buildcheck/trace/1")), Units: []frame.Unit{{Declaration: frame.UnitDeclaration{ID: "trace", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "trace", Contract: contracts.Around.Ref()}}}, Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
			return []frame.ExportValue{
				frame.Contribute("trace", contracts.Around, AroundMetadata{Name: "trace", Order: 1}, Around(func(ctx context.Context, input SourceRevision, next Next) (Result, error) {
					if next == nil {
						return Result{}, errors.New("buildcheck: continuation is required")
					}
					result, err := next(ctx, input)
					result.Trace = append([]string{"trace:enter"}, result.Trace...)
					result.Trace = append(result.Trace, "trace:leave")
					return result, err
				})),
			}, nil
		}}}},
	}
	plugins := make([]*frame.Plugin, 0, len(definitions))
	for _, definition := range definitions {
		plugin, err := frame.Define(definition)
		if err != nil {
			return nil, err
		}
		plugins = append(plugins, plugin)
	}
	return plugins, nil
}
