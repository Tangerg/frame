// Package buildcheck is an independent consumer of Frame. Source revisions,
// compilation phases and diagnostic policies belong to this application.
package buildcheck

import (
	"context"
	"errors"
	"sync"

	framehost "github.com/Tangerg/frame/host"
)

var ErrStaleRevision = errors.New("buildcheck: source revision changed")

type SourceRevision struct {
	Revision uint64 `json:"revision"`
	Text     string `json:"text"`
}

type Source struct {
	mu       sync.Mutex
	revision uint64
	text     string
}

func NewSource(text string) *Source { return &Source{revision: 1, text: text} }
func (s *Source) Read() SourceRevision {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SourceRevision{Revision: s.revision, Text: s.text}
}
func (s *Source) Replace(text string) SourceRevision {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revision++
	s.text = text
	return SourceRevision{Revision: s.revision, Text: s.text}
}
func (s *Source) Apply(patch Patch) (SourceRevision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revision != patch.ExpectedRevision {
		return SourceRevision{}, ErrStaleRevision
	}
	s.revision++
	s.text = patch.Text
	return SourceRevision{Revision: s.revision, Text: s.text}, nil
}

type Diagnostic struct {
	Analyzer string `json:"analyzer"`
	Message  string `json:"message"`
	Line     int    `json:"line"`
}
type Patch struct {
	ExpectedRevision uint64 `json:"expectedRevision"`
	Text             string `json:"text"`
}
type Result struct {
	Source      SourceRevision `json:"source"`
	Output      string         `json:"output"`
	Diagnostics []Diagnostic   `json:"diagnostics"`
	Failures    []string       `json:"failures"`
	Patch       Patch          `json:"patch"`
	Trace       []string       `json:"trace"`
}

type CompilerPass func(context.Context, string) (string, error)
type Analyzer func(context.Context, SourceRevision) ([]Diagnostic, error)
type Formatter func(context.Context, SourceRevision) (Patch, error)
type Next func(context.Context, SourceRevision) (Result, error)
type Around func(context.Context, SourceRevision, Next) (Result, error)

type PassMetadata struct {
	Stage string `json:"stage"`
	Order int    `json:"order"`
}
type AnalyzerMetadata struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
}
type FormatterMetadata struct {
	Name     string `json:"name"`
	Priority int    `json:"priority"`
}
type AroundMetadata struct {
	Name  string `json:"name"`
	Order int    `json:"order"`
}

type Contracts struct {
	Passes     framehost.Point[PassMetadata, CompilerPass]
	Analyzers  framehost.Point[AnalyzerMetadata, Analyzer]
	Formatters framehost.Point[FormatterMetadata, Formatter]
	Around     framehost.Point[AroundMetadata, Around]
}

func NewContracts() (Contracts, error) {
	var contracts Contracts
	var err error
	contracts.Passes, err = framehost.NewPoint[PassMetadata, CompilerPass]("build.compiler-passes", "1")
	if err != nil {
		return contracts, err
	}
	contracts.Analyzers, err = framehost.NewPoint[AnalyzerMetadata, Analyzer]("build.analyzers", "1")
	if err != nil {
		return contracts, err
	}
	contracts.Formatters, err = framehost.NewPoint[FormatterMetadata, Formatter]("build.formatters", "1")
	if err != nil {
		return contracts, err
	}
	contracts.Around, err = framehost.NewPoint[AroundMetadata, Around]("build.around", "1")
	return contracts, err
}
