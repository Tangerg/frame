// Package native demonstrates an authored document plugin using Frame's public
// contracts. It does not use the document workbench's built-in implementations.
package native

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"html"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/examples/document"
	framevalue "github.com/Tangerg/frame/value"
)

const maxSourceBytes = 1 << 20

//go:embed plugins.go
var pluginSource []byte

type Configuration struct {
	SourceDirectory string               `json:"sourceDirectory"`
	WorkDirectory   string               `json:"workDirectory"`
	Template        document.DocumentRef `json:"template"`
}

func configuration(setup *frame.Setup) (Configuration, error) {
	var value Configuration
	if err := json.Unmarshal(setup.Configuration(), &value); err != nil {
		return value, err
	}
	if value.SourceDirectory == "" || value.WorkDirectory == "" {
		return value, errors.New("authoring: source and work directories are required")
	}
	return value, nil
}

// Plugin uses the bytes of the authored implementation as its release identity.
// Contract names and identities come from their existing document owner.
func Plugin(contracts document.Contracts) (*frame.Plugin, error) {
	readOut := frame.Export("read", contracts.Files)
	fileUnit, err := frame.DefineUnit(frame.UnitSpec{ID: "files", Exports: frame.Exports(readOut), Prepare: func(setup *frame.Setup) error {
		config, err := configuration(setup)
		if err != nil {
			return err
		}
		root, _, err := frame.AcquireResource(setup, "authored file root", func(context.Context) (*os.Root, error) { return os.OpenRoot(config.SourceDirectory) }, (*os.Root).Close)
		if err != nil {
			return err
		}
		read := document.FileReader(func(ctx context.Context, ref document.DocumentRef) (document.Document, error) {
			return readSource(ctx, root, ref)
		})
		return readOut.Bind(setup, read)
	}})
	if err != nil {
		return nil, err
	}

	filesSlot := frame.Required("files", contracts.Files)
	upper := frame.Offer("uppercase", contracts.Converters)
	converterUnit, err := frame.DefineUnit(frame.UnitSpec{ID: "converter", Imports: frame.Imports(filesSlot), Exports: frame.Exports(upper), Prepare: func(setup *frame.Setup) error {
		config, err := configuration(setup)
		if err != nil {
			return err
		}
		files, err := filesSlot.Get(setup)
		if err != nil {
			return err
		}
		workspace, _, err := frame.AcquireResource(setup, "authored converter workspace", func(context.Context) (string, error) { return os.MkdirTemp(config.WorkDirectory, "converter-") }, os.RemoveAll)
		if err != nil {
			return err
		}
		// Loading a missing or stale template fails after a real resource
		// was acquired. Its original Setup still owns the workspace.
		template, err := files(setup.Context(), config.Template)
		if err != nil {
			return err
		}
		if strings.Count(template.Text, "{{content}}") != 1 {
			return errors.New("authoring: template requires one content slot")
		}
		preparedTemplate := filepath.Join(workspace, "template.html")
		if err := os.WriteFile(preparedTemplate, []byte(template.Text), 0600); err != nil {
			return err
		}
		convert := document.Converter(func(ctx context.Context, ref document.DocumentRef) (document.Converted, error) {
			source, err := files(ctx, ref)
			if err != nil {
				return document.Converted{}, err
			}
			compiled, err := os.ReadFile(preparedTemplate)
			if err != nil {
				return document.Converted{}, err
			}
			content := html.EscapeString(strings.ToUpper(source.Text))
			output := strings.Replace(string(compiled), "{{content}}", content, 1)
			if err := context.Cause(ctx); err != nil {
				return document.Converted{}, err
			}
			return document.Converted{Source: ref, HTML: output, Title: strings.TrimSpace(strings.SplitN(source.Text, "\n", 2)[0])}, nil
		})
		return upper.Bind(setup, document.ConverterMetadata{Format: "text", Priority: 50}, convert)
	}})
	if err != nil {
		return nil, err
	}

	nonemptyOut := frame.Offer("nonempty", contracts.Validators)
	titleOut := frame.Offer("title", contracts.Validators)
	validatorUnit, err := frame.DefineUnit(frame.UnitSpec{ID: "validation", Exports: frame.Exports(nonemptyOut, titleOut), Prepare: func(setup *frame.Setup) error {
		nonempty := document.Validator(func(ctx context.Context, value document.Converted) ([]document.Diagnostic, error) {
			if err := context.Cause(ctx); err != nil {
				return nil, err
			}
			if strings.TrimSpace(value.HTML) == "" {
				return []document.Diagnostic{{Rule: "nonempty", Message: "conversion produced no output"}}, nil
			}
			return nil, nil
		})
		title := document.Validator(func(ctx context.Context, value document.Converted) ([]document.Diagnostic, error) {
			if err := context.Cause(ctx); err != nil {
				return nil, err
			}
			if len([]rune(value.Title)) < 8 {
				return []document.Diagnostic{{Rule: "title", Message: "title is shorter than eight characters"}}, nil
			}
			return nil, nil
		})
		if err := nonemptyOut.Bind(setup, document.ValidatorMetadata{Name: "nonempty", Order: 10}, nonempty); err != nil {
			return err
		}
		return titleOut.Bind(setup, document.ValidatorMetadata{Name: "title", Order: 20}, title)
	}})
	if err != nil {
		return nil, err
	}
	return frame.Define(frame.Definition{Revision: framevalue.Hash(pluginSource), Units: []frame.Unit{fileUnit, converterUnit, validatorUnit}})
}

func readSource(ctx context.Context, root *os.Root, ref document.DocumentRef) (_ document.Document, err error) {
	if !fs.ValidPath(ref.Path) || ref.Path == "." || ref.Revision.Validate() != nil {
		return document.Document{}, errors.New("authoring: exact source identity is required")
	}
	if err := context.Cause(ctx); err != nil {
		return document.Document{}, err
	}
	file, err := root.Open(ref.Path)
	if err != nil {
		return document.Document{}, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return document.Document{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxSourceBytes {
		return document.Document{}, errors.New("authoring: source must be a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxSourceBytes+1))
	if err != nil {
		return document.Document{}, err
	}
	if len(data) > maxSourceBytes {
		return document.Document{}, errors.New("authoring: source byte budget exceeded")
	}
	if framevalue.Hash(data) != ref.Revision {
		return document.Document{}, document.ErrSourceChanged
	}
	if err := context.Cause(ctx); err != nil {
		return document.Document{}, err
	}
	return document.Document{Ref: ref, Text: string(data)}, nil
}
