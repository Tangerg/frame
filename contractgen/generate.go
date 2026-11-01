package contractgen

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type GoOutput struct{ Path, Package, Prefix string }

func Generate(source, directory, tsDirectory string, goOutputs ...GoOutput) error {
	type artifact struct {
		path string
		body []byte
	}
	var outputs []artifact
	appendOutput := func(path string, body []byte, err error) error {
		if err != nil {
			return err
		}
		outputs = append(outputs, artifact{path, body})
		return nil
	}
	var input []byte
	var err error
	if source == "-" {
		input, err = io.ReadAll(os.Stdin)
	} else {
		input, err = os.ReadFile(source)
	}
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
	// Admit every target before writing; a hard link is the same source despite a different path.
	targets := map[string]bool{}
	for _, output := range outputs {
		target, err := filepath.Abs(output.path)
		if err != nil {
			return err
		}
		if targets[target] {
			return fmt.Errorf("feature contracts: duplicate output %s", target)
		}
		targets[target] = true
		if source != "-" {
			inputPath, err := filepath.Abs(source)
			if err != nil {
				return err
			}
			original, err := os.Stat(source)
			if err != nil {
				return err
			}
			existing, err := os.Stat(target)
			if err != nil && !os.IsNotExist(err) {
				return err
			}
			if inputPath == target || err == nil && os.SameFile(original, existing) {
				return fmt.Errorf("feature contracts: output would overwrite descriptor input %s", source)
			}
		}
	}
	for _, output := range outputs {
		if err := os.MkdirAll(filepath.Dir(output.path), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(output.path, output.body, 0644); err != nil {
			return err
		}
	}
	return nil
}
