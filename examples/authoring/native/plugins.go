// Package native demonstrates an authored document plugin using Frame's public
// contracts. It does not use the document workbench's built-in implementations.
package native

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/examples/document"
	framehost "github.com/Tangerg/frame/host"
	framevalue "github.com/Tangerg/frame/value"
)

const (
	maxSourceBytes                         = 1 << 20
	minimumTitleRunes                      = 8
	nonemptyValidationOrder                = 10
	titleValidationOrder                   = 20
	uppercaseConverterPriority             = 50
	textFormat                             = "text"
	contentSlot                            = "{{content}}"
	templateFileMode           fs.FileMode = 0o600
)

//go:embed plugins.go release.go source_flags_*.go
var pluginSources embed.FS

type Configuration struct {
	SourceDirectory string               `json:"sourceDirectory"`
	WorkDirectory   string               `json:"workDirectory"`
	Template        document.DocumentRef `json:"template"`
}

func configuration(setup *framehost.Setup) (Configuration, error) {
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
func Plugin(contracts document.Contracts) (*framehost.Plugin, error) {
	readOut := framehost.Export("read", contracts.Files)
	fileUnit, err := framehost.DefineUnit(framehost.UnitSpec{ID: "files", Exports: framehost.Exports(readOut), Prepare: func(setup *framehost.Setup) error {
		config, err := configuration(setup)
		if err != nil {
			return err
		}
		root, _, err := framehost.AcquireResource(setup, "authored file root", func(context.Context) (*os.Root, error) { return os.OpenRoot(config.SourceDirectory) }, (*os.Root).Close)
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

	filesSlot := framehost.Required("files", contracts.Files)
	upper := framehost.Offer("uppercase", contracts.Converters)
	converterUnit, err := framehost.DefineUnit(framehost.UnitSpec{ID: "converter", Imports: framehost.Imports(filesSlot), Exports: framehost.Exports(upper), Prepare: func(setup *framehost.Setup) error {
		config, err := configuration(setup)
		if err != nil {
			return err
		}
		files, err := filesSlot.Get(setup)
		if err != nil {
			return err
		}
		workspace, _, err := framehost.AcquireResource(setup, "authored converter workspace", func(context.Context) (string, error) { return os.MkdirTemp(config.WorkDirectory, "converter-") }, os.RemoveAll)
		if err != nil {
			return err
		}
		// Loading a missing or stale template fails after a real resource
		// was acquired. Its original Setup still owns the workspace.
		template, err := files(setup.Context(), config.Template)
		if err != nil {
			return err
		}
		if strings.Count(template.Text, contentSlot) != 1 {
			return errors.New("authoring: template requires one content slot")
		}
		preparedTemplate := filepath.Join(workspace, "template.html")
		if err := os.WriteFile(preparedTemplate, []byte(template.Text), templateFileMode); err != nil {
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
			output := strings.Replace(string(compiled), contentSlot, content, 1)
			if err := frame.ContextError(ctx); err != nil {
				return document.Converted{}, err
			}
			return document.Converted{Source: ref, HTML: output, Title: strings.TrimSpace(strings.SplitN(source.Text, "\n", 2)[0])}, nil
		})
		return upper.Bind(setup, document.ConverterMetadata{Format: textFormat, Priority: uppercaseConverterPriority}, convert)
	}})
	if err != nil {
		return nil, err
	}

	nonempty := validationRule{
		metadata:   document.ValidatorMetadata{Name: "nonempty", Order: nonemptyValidationOrder},
		message:    "conversion produced no output",
		violatedBy: func(value document.Converted) bool { return strings.TrimSpace(value.HTML) == "" },
	}
	title := validationRule{
		metadata:   document.ValidatorMetadata{Name: "title", Order: titleValidationOrder},
		message:    fmt.Sprintf("title is shorter than %d characters", minimumTitleRunes),
		violatedBy: func(value document.Converted) bool { return utf8.RuneCountInString(value.Title) < minimumTitleRunes },
	}
	nonemptyOut := framehost.Offer(nonempty.metadata.Name, contracts.Validators)
	titleOut := framehost.Offer(title.metadata.Name, contracts.Validators)
	validatorUnit, err := framehost.DefineUnit(framehost.UnitSpec{ID: "validation", Exports: framehost.Exports(nonemptyOut, titleOut), Prepare: func(setup *framehost.Setup) error {
		if err := nonemptyOut.Bind(setup, nonempty.metadata, document.Validator(nonempty.validate)); err != nil {
			return err
		}
		return titleOut.Bind(setup, title.metadata, document.Validator(title.validate))
	}})
	if err != nil {
		return nil, err
	}
	revision, err := pluginRevision()
	if err != nil {
		return nil, err
	}
	return framehost.Define(framehost.Definition{Revision: revision, Units: []framehost.Unit{fileUnit, converterUnit, validatorUnit}})
}

type validationRule struct {
	metadata   document.ValidatorMetadata
	message    string
	violatedBy func(document.Converted) bool
}

func (r validationRule) validate(ctx context.Context, value document.Converted) ([]document.Diagnostic, error) {
	if err := frame.ContextError(ctx); err != nil {
		return nil, err
	}
	if r.violatedBy(value) {
		return []document.Diagnostic{{Rule: r.metadata.Name, Message: r.message}}, nil
	}
	return nil, nil
}

func readSource(ctx context.Context, root *os.Root, ref document.DocumentRef) (_ document.Document, err error) {
	if !fs.ValidPath(ref.Path) || ref.Path == "." || ref.Revision.Validate() != nil {
		return document.Document{}, errors.New("authoring: exact source identity is required")
	}
	if err := frame.ContextError(ctx); err != nil {
		return document.Document{}, err
	}
	file, err := root.OpenFile(ref.Path, os.O_RDONLY|sourceOpenFlags, 0)
	if err != nil {
		return document.Document{}, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return document.Document{}, err
	}
	if !info.Mode().IsRegular() {
		return document.Document{}, &fs.PathError{Op: "read source", Path: ref.Path, Err: fs.ErrInvalid}
	}
	if info.Size() > maxSourceBytes {
		return document.Document{}, errors.New("authoring: source byte budget exceeded")
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
	if err := frame.ContextError(ctx); err != nil {
		return document.Document{}, err
	}
	return document.Document{Ref: ref, Text: string(data)}, nil
}
