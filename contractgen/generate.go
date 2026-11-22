// Package contractgen publishes Go, TypeScript and JSON Schema projections of a
// portable capability descriptor. It is a publisher tool, never a runtime
// dependency of a Host.
package contractgen

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Tangerg/frame/internal/localfile"
	framevalue "github.com/Tangerg/frame/value"
)

type GoOutput struct{ Path, Package, Prefix string }

// GenerateConfig selects the projections of one published capability
// descriptor. An empty directory requests none of that directory's files.
type GenerateConfig struct {
	// Source is a descriptor path, or StandardInput.
	Source              string
	ArtifactDirectory   string
	TypeScriptDirectory string
	Go                  []GoOutput
}

// StandardInput names the process's standard input as the descriptor source.
const StandardInput = "-"

const (
	methodsFile    = "methods.generated.json"
	capabilityFile = "capability.generated.json"
	referenceFile  = "API_REFERENCE.md"
	typeScriptFile = "methods.generated.ts"
)

var ErrInvalid = errors.New("contractgen: invalid input")

// Callers own the selected destinations for the duration of generation. Files in
// different directories cannot form a single filesystem transaction for readers.
func Generate(config GenerateConfig) error {
	if config.Source == "" {
		return fmt.Errorf("%w: descriptor source is required", ErrInvalid)
	}
	for _, output := range config.Go {
		if output.Path == "" {
			return fmt.Errorf("%w: Go projection path is required", ErrInvalid)
		}
	}
	var outputs []artifact
	appendOutput := func(path string, body []byte, err error) error {
		if err != nil {
			return err
		}
		outputs = append(outputs, artifact{path, body})
		return nil
	}
	reader := os.Stdin
	if config.Source != StandardInput {
		file, err := os.Open(config.Source)
		if err != nil {
			return err
		}
		defer file.Close()
		reader = file
	}
	input, err := io.ReadAll(io.LimitReader(reader, int64(framevalue.PortableLimits().MaxDescriptorBytes)+1))
	if err != nil {
		return err
	}
	feature, err := buildFeature(input)
	if err != nil {
		return err
	}
	if config.ArtifactDirectory != "" {
		for _, output := range []struct {
			name   string
			encode func() ([]byte, error)
		}{
			{schemaFile, feature.schemaJSON},
			{methodsFile, feature.methodsJSON},
			{capabilityFile, feature.capabilityJSON},
		} {
			body, err := output.encode()
			if err := appendOutput(filepath.Join(config.ArtifactDirectory, output.name), body, err); err != nil {
				return err
			}
		}
		outputs = append(outputs, artifact{filepath.Join(config.ArtifactDirectory, referenceFile), []byte(feature.reference())})
	}
	if config.TypeScriptDirectory != "" {
		outputs = append(outputs, artifact{filepath.Join(config.TypeScriptDirectory, typeScriptFile), []byte(feature.typeScript())})
	}
	for _, output := range config.Go {
		body, err := feature.goProjection(featureGoOptions{Package: output.Package, Prefix: output.Prefix})
		if err := appendOutput(output.Path, body, err); err != nil {
			return err
		}
	}
	return publishArtifacts(config.Source, outputs)
}

type artifact struct {
	path string
	body []byte
}

// A stage directory holds the next file and a link to the original one.
const (
	stagePattern      = ".frame-contract-"
	nextStageName     = "next"
	originalStageName = "original"
)

type stagedArtifact struct {
	target   string
	original os.FileInfo
	stage    string
}

// Generated files can span filesystems. Stage beside each destination and retain
// its original inode until all renames succeed, so an ordinary failure is reversible.
func publishArtifacts(source string, outputs []artifact) (err error) {
	stages, err := planStages(source, outputs)
	if err != nil {
		return err
	}
	defer func() {
		for _, s := range stages {
			if s.stage != "" {
				err = errors.Join(err, os.RemoveAll(s.stage))
			}
		}
	}()
	for i := range stages {
		if err := stages[i].prepare(outputs[i].body); err != nil {
			return err
		}
	}
	return commitArtifacts(stages)
}

// planStages resolves each output to a distinct regular file that is neither
// the descriptor input, another output, nor the parent of another output.
func planStages(source string, outputs []artifact) ([]stagedArtifact, error) {
	var input os.FileInfo
	if source != StandardInput {
		var err error
		if input, err = os.Stat(source); err != nil {
			return nil, err
		}
	}
	targets := make(map[string]bool, len(outputs))
	stages := make([]stagedArtifact, 0, len(outputs))
	for _, output := range outputs {
		stage, err := resolveStage(output.path)
		if err != nil {
			return nil, err
		}
		if targets[stage.target] {
			return nil, fmt.Errorf("%w: duplicate output %s", ErrInvalid, stage.target)
		}
		targets[stage.target] = true
		if err := stage.checkDistinct(source, input, stages); err != nil {
			return nil, err
		}
		stages = append(stages, stage)
	}
	for _, s := range stages {
		for parent := filepath.Dir(s.target); parent != filepath.Dir(parent); parent = filepath.Dir(parent) {
			if targets[parent] {
				return nil, fmt.Errorf("%w: output is also a parent: %s", ErrInvalid, parent)
			}
		}
	}
	return stages, nil
}

func resolveStage(path string) (stagedArtifact, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return stagedArtifact{}, err
	}
	parent, err := canonicalParent(filepath.Dir(absolute))
	if err != nil {
		return stagedArtifact{}, err
	}
	target := filepath.Join(parent, filepath.Base(absolute))
	info, err := os.Lstat(target)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return stagedArtifact{}, err
	}
	if info != nil && !info.Mode().IsRegular() {
		return stagedArtifact{}, fmt.Errorf("%w: output is not a regular file: %s", ErrInvalid, target)
	}
	return stagedArtifact{target: target, original: info}, nil
}

// checkDistinct refuses an existing output that aliases the input or an
// earlier output through a hard link.
func (s stagedArtifact) checkDistinct(source string, input os.FileInfo, earlier []stagedArtifact) error {
	if s.original == nil {
		return nil
	}
	if input != nil && os.SameFile(input, s.original) {
		return fmt.Errorf("%w: output would overwrite descriptor input %s", ErrInvalid, source)
	}
	for _, prior := range earlier {
		if prior.original != nil && os.SameFile(s.original, prior.original) {
			return fmt.Errorf("%w: aliased outputs %s and %s", ErrInvalid, prior.target, s.target)
		}
	}
	return nil
}

func commitArtifacts(stages []stagedArtifact) error {
	for i := range stages {
		s := &stages[i]
		if err := os.Rename(filepath.Join(s.stage, nextStageName), s.target); err != nil {
			for j := i - 1; j >= 0; j-- {
				err = errors.Join(err, stages[j].restore())
			}
			return err
		}
	}
	return nil
}

func (s *stagedArtifact) restore() error {
	if s.original == nil {
		return os.Remove(s.target)
	}
	if err := os.Rename(filepath.Join(s.stage, originalStageName), s.target); err != nil {
		// Preserve the backup when the filesystem refuses rollback as well.
		backup := s.stage
		s.stage = ""
		return fmt.Errorf("contractgen: restore %s from %s: %w", s.target, backup, err)
	}
	return nil
}

func canonicalParent(directory string) (string, error) {
	resolved, err := filepath.EvalSymlinks(directory)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	// Preserve not-yet-created suffixes while resolving aliases in the existing prefix.
	if _, e := os.Lstat(directory); e == nil || filepath.Dir(directory) == directory {
		return "", err
	}
	parent, err := canonicalParent(filepath.Dir(directory))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(directory)), nil
}

func (s *stagedArtifact) prepare(body []byte) error {
	directory := filepath.Dir(s.target)
	if err := os.MkdirAll(directory, localfile.PublicDirectoryMode); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(directory, stagePattern)
	if err != nil {
		return err
	}
	s.stage = stage
	mode := localfile.PublicFileMode
	if s.original != nil {
		if err := os.Link(s.target, filepath.Join(stage, originalStageName)); err != nil {
			return err
		}
		mode = s.original.Mode().Perm()
	}
	return os.WriteFile(filepath.Join(stage, nextStageName), body, mode)
}
