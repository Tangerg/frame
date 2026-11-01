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

type FileConfiguration struct {
	Directory string `json:"directory"`
}

func FilePlugin(contracts Contracts) (*frame.Plugin, error) {
	return frame.Define(frame.Definition{Revision: value.Hash([]byte("document/file-reader/1")), Units: []frame.Unit{{Declaration: frame.UnitDeclaration{ID: "files", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "read", Contract: contracts.Files.Ref()}}}, Setup: func(ctx context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
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
		return []frame.ExportValue{frame.Provide("read", contracts.Files, FileReader(func(ctx context.Context, ref DocumentRef) (Document, error) { return readDocument(ctx, root, ref) }))}, nil
	}}}})
}

func ConverterPlugin(contracts Contracts, revision string) (*frame.Plugin, error) {
	return frame.Define(frame.Definition{Revision: value.Hash([]byte("document/converter/" + revision)), Units: []frame.Unit{{Declaration: frame.UnitDeclaration{ID: "converter", Profile: frame.NativeGo, Imports: []frame.ImportDeclaration{{Slot: "files", Contract: contracts.Files.Ref()}}, Exports: []frame.ExportDeclaration{{Name: "markdown", Contract: contracts.Converters.Ref()}}}, Setup: func(ctx context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
		files, err := frame.Import(setup, "files", contracts.Files)
		if err != nil {
			return nil, err
		}
		convert := Converter(func(ctx context.Context, reference DocumentRef) (Converted, error) {
			document, err := files(ctx, reference)
			if err != nil {
				return Converted{}, err
			}
			result := Converted{Source: reference, Title: reference.Path}
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
		return []frame.ExportValue{frame.Contribute("markdown", contracts.Converters, ConverterMetadata{Format: "markdown", Priority: 10}, convert)}, nil
	}}}})
}

func ValidationPlugin(contracts Contracts) (*frame.Plugin, error) {
	return frame.Define(frame.Definition{Revision: value.Hash([]byte("document/validation/1")), Units: []frame.Unit{{Declaration: frame.UnitDeclaration{ID: "validation", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "title", Contract: contracts.Validators.Ref()}}}, Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
		return []frame.ExportValue{frame.Contribute("title", contracts.Validators, ValidatorMetadata{Name: "title", Order: 10}, Validator(func(ctx context.Context, converted Converted) ([]Diagnostic, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if converted.Title == converted.Source.Path {
				return []Diagnostic{{Rule: "title", Message: "document has no level-one heading"}}, nil
			}
			return nil, nil
		}))}, nil
	}}}})
}
