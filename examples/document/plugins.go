package document

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"os"
	"strings"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

const (
	fileReaderUnitID            frame.UnitID = "files"
	fileReadExportName                       = "read"
	fileReaderRevision                       = "document/file-reader/1"
	converterUnitID             frame.UnitID = "converter"
	converterRevisionPrefix                  = "document/converter/"
	fileImportSlot                           = "files"
	markdownExportName                       = "markdown"
	markdownFormat                           = "markdown"
	defaultMarkdownPriority                  = 10
	validationUnitID            frame.UnitID = "validation"
	validationRevision                       = "document/validation/1"
	titleValidatorExportName                 = "title"
	titleRuleName                            = "title"
	defaultTitleValidationOrder              = 10
)

type FileConfiguration struct {
	Directory string `json:"directory"`
}

func FilePlugin(contracts Contracts) (*frame.Plugin, error) {
	unit := frame.Unit{
		Declaration: frame.UnitDeclaration{
			ID:      fileReaderUnitID,
			Profile: frame.NativeGo,
			Exports: []frame.ExportDeclaration{{Name: fileReadExportName, Contract: contracts.Files.Ref()}},
		},
		Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
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
			return []frame.ExportValue{frame.Provide(fileReadExportName, contracts.Files, FileReader(func(ctx context.Context, ref DocumentRef) (Document, error) { return readDocument(ctx, root, ref) }))}, nil
		},
	}
	return frame.Define(frame.Definition{Revision: value.Hash([]byte(fileReaderRevision)), Units: []frame.Unit{unit}})
}

func ConverterPlugin(contracts Contracts, revision string) (*frame.Plugin, error) {
	unit := frame.Unit{
		Declaration: frame.UnitDeclaration{
			ID:      converterUnitID,
			Profile: frame.NativeGo,
			Imports: []frame.ImportDeclaration{{Slot: fileImportSlot, Contract: contracts.Files.Ref()}},
			Exports: []frame.ExportDeclaration{{Name: markdownExportName, Contract: contracts.Converters.Ref()}},
		},
		Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
			files, err := frame.Import(setup, fileImportSlot, contracts.Files)
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
					if err := ctx.Err(); err != nil {
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
			return []frame.ExportValue{frame.Contribute(markdownExportName, contracts.Converters, ConverterMetadata{Format: markdownFormat, Priority: defaultMarkdownPriority}, convert)}, nil
		},
	}
	return frame.Define(frame.Definition{Revision: value.Hash([]byte(converterRevisionPrefix + revision)), Units: []frame.Unit{unit}})
}

func ValidationPlugin(contracts Contracts) (*frame.Plugin, error) {
	unit := frame.Unit{
		Declaration: frame.UnitDeclaration{
			ID:      validationUnitID,
			Profile: frame.NativeGo,
			Exports: []frame.ExportDeclaration{{Name: titleValidatorExportName, Contract: contracts.Validators.Ref()}},
		},
		Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
			return []frame.ExportValue{frame.Contribute(titleValidatorExportName, contracts.Validators, ValidatorMetadata{Name: titleRuleName, Order: defaultTitleValidationOrder}, Validator(func(ctx context.Context, converted Converted) ([]Diagnostic, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if converted.Title == "" {
					return []Diagnostic{{Rule: titleRuleName, Message: "document has no level-one heading"}}, nil
				}
				return nil, nil
			}))}, nil
		},
	}
	return frame.Define(frame.Definition{Revision: value.Hash([]byte(validationRevision)), Units: []frame.Unit{unit}})
}
