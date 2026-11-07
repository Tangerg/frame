// Package document is a document-workbench consumer of Frame. Document identity,
// source revisions, conversion policy and task results belong to this application.
package document

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
	"github.com/Tangerg/frame/distribution"
	"github.com/Tangerg/frame/internal/cancellation"
	"github.com/Tangerg/frame/internal/localfile"
	"github.com/Tangerg/frame/value"
)

var ErrSourceChanged = errors.New("document: source revision changed")
var ErrTaskFinished = errors.New("document: task is already terminal")

type DocumentRef struct {
	Path     string       `json:"path"`
	Revision value.Digest `json:"revision"`
}
type Document struct {
	Ref  DocumentRef `json:"ref"`
	Text string      `json:"text"`
}
type Converted struct {
	Source DocumentRef `json:"source"`
	HTML   string      `json:"html"`
	Title  string      `json:"title"`
}
type Diagnostic struct {
	Rule    string `json:"rule"`
	Message string `json:"message"`
}
type ConverterMetadata struct {
	Format   string `json:"format"`
	Priority int    `json:"priority"`
}
type ValidatorMetadata struct {
	Name  string `json:"name"`
	Order int    `json:"order"`
}
type PreviewMetadata struct {
	Format   string `json:"format"`
	Priority int    `json:"priority"`
}
type FileReader func(context.Context, DocumentRef) (Document, error)
type Converter func(context.Context, DocumentRef) (Converted, error)
type Validator func(context.Context, Converted) ([]Diagnostic, error)

type Contracts struct {
	Files      frame.Contract[FileReader]
	Converters frame.Point[ConverterMetadata, Converter]
	Validators frame.Point[ValidatorMetadata, Validator]
	Previews   frame.Point[PreviewMetadata, browser.Object]
	Theme      frame.Contract[*distribution.Resources]
}

func NewContracts() (Contracts, error) {
	var contracts Contracts
	var err error
	contracts.Files, err = frame.NewContract[FileReader]("documents.file-reader", "1")
	if err != nil {
		return contracts, err
	}
	contracts.Converters, err = frame.NewPoint[ConverterMetadata, Converter]("documents.converters", "1")
	if err != nil {
		return contracts, err
	}
	contracts.Validators, err = frame.NewPoint[ValidatorMetadata, Validator]("documents.validators", "1")
	if err != nil {
		return contracts, err
	}
	contracts.Previews, err = frame.NewPoint[PreviewMetadata, browser.Object]("documents.previews", "1")
	if err != nil {
		return contracts, err
	}
	contracts.Theme, err = frame.NewContract[*distribution.Resources]("documents.theme", "1")
	return contracts, err
}

func readDocument(ctx context.Context, root *os.Root, reference DocumentRef) (Document, error) {
	if reference.Revision.Validate() != nil {
		return Document{}, errors.New("document: exact source identity is required")
	}
	source, err := loadDocument(ctx, root, reference.Path)
	if err != nil {
		return Document{}, err
	}
	if source.Ref.Revision != reference.Revision {
		return Document{}, ErrSourceChanged
	}
	return source, nil
}

const maximumDocumentBytes = 4 << 20

func loadDocument(ctx context.Context, root *os.Root, path string) (Document, error) {
	if !fs.ValidPath(path) || path == "." {
		return Document{}, frame.ErrInvalid
	}
	file, err := localfile.OpenRegularIn(root, path)
	if err != nil {
		return Document{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Document{}, err
	}
	if info.Size() > maximumDocumentBytes {
		return Document{}, frame.ErrCapacity
	}
	if err := cancellation.Err(ctx); err != nil {
		return Document{}, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maximumDocumentBytes+1))
	if err != nil {
		return Document{}, err
	}
	if len(data) > maximumDocumentBytes {
		return Document{}, frame.ErrCapacity
	}
	if err := cancellation.Err(ctx); err != nil {
		return Document{}, err
	}
	return Document{Ref: DocumentRef{Path: path, Revision: value.Hash(data)}, Text: string(data)}, nil
}
