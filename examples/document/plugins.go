package document

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"html"
	"os"
	"strings"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

const (
	fileReaderUnitID            framehost.UnitID = "files"
	fileReadExportName                           = "read"
	fileReaderRevision                           = "document/file-reader/1"
	converterUnitID             framehost.UnitID = "converter"
	converterRevisionPrefix                      = "document/converter/"
	fileImportSlot                               = "files"
	markdownExportName                           = "markdown"
	markdownFormat                               = "markdown"
	defaultMarkdownPriority                      = 10
	validationUnitID            framehost.UnitID = "validation"
	validationRevision                           = "document/validation/1"
	titleValidatorExportName                     = "title"
	titleRuleName                                = "title"
	defaultTitleValidationOrder                  = 10
)

type FileConfiguration struct {
	Directory string `json:"directory"`
}

func FilePlugin(contracts Contracts) (*framehost.Plugin, error) {
	unit := framehost.Unit{
		Declaration: framehost.UnitDeclaration{
			ID:      fileReaderUnitID,
			Profile: framehost.NativeGo,
			Exports: []framehost.ExportDeclaration{{Name: fileReadExportName, Contract: contracts.Files.Ref()}},
		},
		Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
			var configuration FileConfiguration
			if err := json.Unmarshal(setup.Configuration(), &configuration); err != nil {
				return nil, err
			}
			root, err := os.OpenRoot(configuration.Directory)
			if err != nil {
				return nil, err
			}
			if _, err := setup.Cleanup("document files", root.Close); err != nil {
				return nil, errors.Join(err, root.Close())
			}
			return []framehost.ExportValue{framehost.Provide(fileReadExportName, contracts.Files, FileReader(func(ctx context.Context, ref DocumentRef) (Document, error) { return readDocument(ctx, root, ref) }))}, nil
		},
	}
	return framehost.Define(framehost.Definition{Revision: value.Hash([]byte(fileReaderRevision)), Units: []framehost.Unit{unit}})
}

func ConverterPlugin(contracts Contracts, revision string) (*framehost.Plugin, error) {
	unit := framehost.Unit{
		Declaration: framehost.UnitDeclaration{
			ID:      converterUnitID,
			Profile: framehost.NativeGo,
			Imports: []framehost.ImportDeclaration{{Slot: fileImportSlot, Contract: contracts.Files.Ref()}},
			Exports: []framehost.ExportDeclaration{{Name: markdownExportName, Contract: contracts.Converters.Ref()}},
		},
		Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
			files, err := framehost.Import(setup, fileImportSlot, contracts.Files)
			if err != nil {
				return nil, err
			}
			convert := Converter(func(ctx context.Context, reference DocumentRef) (Converted, error) {
				document, err := files(ctx, reference)
				if err != nil {
					return Converted{}, err
				}
				result := Converted{Source: reference}
				var output strings.Builder
				for _, line := range strings.Split(document.Text, "\n") {
					if err := frame.ContextError(ctx); err != nil {
						return Converted{}, err
					}
					switch {
					case strings.HasPrefix(line, "# "):
						title := strings.TrimPrefix(line, "# ")
						result.Title = title
						output.WriteString("<h1>" + html.EscapeString(title) + "</h1>\n")
					case line != "":
						output.WriteString("<p>" + html.EscapeString(line) + "</p>\n")
					}
				}
				result.HTML = output.String()
				return result, nil
			})
			return []framehost.ExportValue{framehost.Contribute(markdownExportName, contracts.Converters, ConverterMetadata{Format: markdownFormat, Priority: defaultMarkdownPriority}, convert)}, nil
		},
	}
	return framehost.Define(framehost.Definition{Revision: value.Hash([]byte(converterRevisionPrefix + revision)), Units: []framehost.Unit{unit}})
}

func ValidationPlugin(contracts Contracts) (*framehost.Plugin, error) {
	unit := framehost.Unit{
		Declaration: framehost.UnitDeclaration{
			ID:      validationUnitID,
			Profile: framehost.NativeGo,
			Exports: []framehost.ExportDeclaration{{Name: titleValidatorExportName, Contract: contracts.Validators.Ref()}},
		},
		Setup: func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) {
			return []framehost.ExportValue{framehost.Contribute(titleValidatorExportName, contracts.Validators, ValidatorMetadata{Name: titleRuleName, Order: defaultTitleValidationOrder}, Validator(func(ctx context.Context, converted Converted) ([]Diagnostic, error) {
				if err := frame.ContextError(ctx); err != nil {
					return nil, err
				}
				if converted.Title == "" {
					return []Diagnostic{{Rule: titleRuleName, Message: "document has no level-one heading"}}, nil
				}
				return nil, nil
			}))}, nil
		},
	}
	return framehost.Define(framehost.Definition{Revision: value.Hash([]byte(validationRevision)), Units: []framehost.Unit{unit}})
}
