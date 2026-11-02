package contractgen

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Tangerg/frame/portable"
)

type GoOutput struct{ Path, Package, Prefix string }

// Callers own the selected destinations for the duration of generation. Files in
// different directories cannot form a single filesystem transaction for readers.
func Generate(source, directory, tsDirectory string, goOutputs ...GoOutput) error {
	var outputs []artifact
	appendOutput := func(path string, body []byte, err error) error {
		if err != nil {
			return err
		}
		outputs = append(outputs, artifact{path, body})
		return nil
	}
	reader := os.Stdin
	if source != "-" {
		file, err := os.Open(source)
		if err != nil {
			return err
		}
		defer file.Close()
		reader = file
	}
	input, err := io.ReadAll(io.LimitReader(reader, int64(portable.ProfileLimits().MaxDescriptorBytes)+1))
	if err != nil {
		return err
	}
	feature, err := buildFeature(input)
	if err != nil {
		return err
	}
	if directory != "" {
		for _, output := range []struct {
			name   string
			encode func() ([]byte, error)
		}{
			{"schema.json", feature.schemaJSON},
			{"methods.generated.json", feature.methodsJSON},
			{"capability.generated.json", feature.capabilityJSON},
		} {
			body, err := output.encode()
			if err := appendOutput(filepath.Join(directory, output.name), body, err); err != nil {
				return err
			}
		}
		outputs = append(outputs, artifact{filepath.Join(directory, "API_REFERENCE.md"), []byte(feature.reference())})
	}
	if tsDirectory != "" {
		outputs = append(outputs, artifact{filepath.Join(tsDirectory, "methods.generated.ts"), []byte(feature.typeScript())})
	}
	for _, output := range goOutputs {
		if output.Path == "" {
			continue
		}
		body, err := feature.goProjection(featureGoOptions{Package: output.Package, Prefix: output.Prefix})
		if err := appendOutput(output.Path, body, err); err != nil {
			return err
		}
	}
	return publishArtifacts(source, outputs)
}

type artifact struct {
	path string
	body []byte
}

type stagedArtifact struct {
	target   string
	original os.FileInfo
	stage    string
}

// Generated files can span filesystems. Stage beside each destination and retain
// its original inode until all renames succeed, so an ordinary failure is reversible.
func publishArtifacts(source string, outputs []artifact) (err error) {
	var input os.FileInfo
	if source != "-" {
		input, err = os.Stat(source)
		if err != nil {
			return err
		}
	}
	targets := make(map[string]bool, len(outputs))
	stages := make([]stagedArtifact, len(outputs))
	for i, output := range outputs {
		absolute, err := filepath.Abs(output.path)
		if err != nil {
			return err
		}
		parent, err := canonicalParent(filepath.Dir(absolute))
		if err != nil {
			return err
		}
		target := filepath.Join(parent, filepath.Base(absolute))
		if targets[target] {
			return fmt.Errorf("feature contracts: duplicate output %s", target)
		}
		targets[target] = true
		info, err := os.Lstat(target)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if info != nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("feature contracts: output is not a regular file: %s", target)
			}
			if input != nil && os.SameFile(input, info) {
				return fmt.Errorf("feature contracts: output would overwrite descriptor input %s", source)
			}
			for _, prior := range stages[:i] {
				if prior.original != nil && os.SameFile(info, prior.original) {
					return fmt.Errorf("feature contracts: aliased outputs %s and %s", prior.target, target)
				}
			}
		}
		stages[i] = stagedArtifact{target: target, original: info}
	}
	for _, s := range stages {
		for parent := filepath.Dir(s.target); parent != filepath.Dir(parent); parent = filepath.Dir(parent) {
			if targets[parent] {
				return fmt.Errorf("feature contracts: output is also a parent: %s", parent)
			}
		}
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

func commitArtifacts(stages []stagedArtifact) error {
	for i := range stages {
		s := &stages[i]
		if err := os.Rename(filepath.Join(s.stage, "next"), s.target); err != nil {
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
	if err := os.Rename(filepath.Join(s.stage, "original"), s.target); err != nil {
		// Preserve the backup when the filesystem refuses rollback as well.
		backup := s.stage
		s.stage = ""
		return fmt.Errorf("feature contracts: restore %s from %s: %w", s.target, backup, err)
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
	if err := os.MkdirAll(directory, 0755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(directory, ".frame-contract-")
	if err != nil {
		return err
	}
	s.stage = stage
	mode := os.FileMode(0644)
	if s.original != nil {
		if err := os.Link(s.target, filepath.Join(stage, "original")); err != nil {
			return err
		}
		mode = s.original.Mode().Perm()
	}
	return os.WriteFile(filepath.Join(stage, "next"), body, mode)
}
