package buildcheck_test

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/examples/buildcheck"
)

func TestAnalyzerCompletion(t *testing.T) {
	for _, required := range []bool{true, false} {
		for _, interrupted := range []bool{false, true} {
			name := "optional/error"
			if required {
				name = "required/error"
			}
			if interrupted {
				name += "/goexit"
			}
			t.Run(name, func(t *testing.T) {
				e, ctx := engine(t, "source input")
				failure := errors.New("analyzer failed")
				install(t, ctx, e, "completion-analyzer", frame.Contribute("completion", e.Contracts.Analyzers,
					buildcheck.AnalyzerMetadata{Name: "completion", Required: required},
					buildcheck.Analyzer(func(context.Context, buildcheck.SourceRevision) ([]buildcheck.Diagnostic, error) {
						if interrupted {
							runtime.Goexit()
						}
						return nil, failure
					})))
				result, err := e.Run(ctx)
				if required && err == nil {
					t.Error("required analyzer exited without returning, but run succeeded")
				}
				if !required && len(result.Failures) == 0 {
					t.Error("optional analyzer exited without returning, but failure was omitted")
				}
				if !interrupted && required && !errors.Is(err, failure) {
					t.Fatal("lost ordinary error control")
				}
			})
		}
	}
}
