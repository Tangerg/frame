package document

import (
	"bytes"
	"context"
	"embed"
	json "encoding/json/v2"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Tangerg/frame/browser"
	"github.com/Tangerg/frame/distribution"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/internal/localfile"
	"github.com/Tangerg/frame/value"
)

//go:embed assets/preview.js assets/theme.css
var assets embed.FS

const (
	artifactPathPrefix                      = "/frame/artifacts/"
	previewPackageName                      = "example.document-preview"
	previewPackageVersion                   = "1"
	previewModulePath                       = "preview.js"
	previewUnitID          framehost.UnitID = "preview"
	previewExportName                       = "preview"
	themeUnitID            framehost.UnitID = "theme"
	themeExportName                         = "theme"
	themeResourcePath                       = "theme.css"
	defaultPreviewPriority                  = 10
)

func buildPreview(ctx context.Context, store distribution.Store, contracts Contracts, authorize func(value.Digest)) (distribution.Release, error) {
	directory, err := os.MkdirTemp("", "frame-document-package-")
	if err != nil {
		return distribution.Release{}, err
	}
	defer os.RemoveAll(directory)
	module, err := assets.ReadFile("assets/" + previewModulePath)
	if err != nil {
		return distribution.Release{}, err
	}
	declaration, err := json.Marshal(struct {
		UnitID   framehost.UnitID      `json:"unitId"`
		Export   string                `json:"export"`
		Point    framehost.ContractRef `json:"point"`
		Metadata PreviewMetadata       `json:"metadata"`
	}{previewUnitID, previewExportName, contracts.Previews.Ref(), PreviewMetadata{Format: "html", Priority: defaultPreviewPriority}})
	if err != nil {
		return distribution.Release{}, err
	}
	module = slices.Concat([]byte("const published = Object.freeze("), declaration, []byte(");\n"), module)
	if err := os.WriteFile(filepath.Join(directory, previewModulePath), module, localfile.PrivateFileMode); err != nil {
		return distribution.Release{}, err
	}
	theme, err := assets.ReadFile("assets/" + themeResourcePath)
	if err != nil {
		return distribution.Release{}, err
	}
	if err := os.WriteFile(filepath.Join(directory, themeResourcePath), theme, localfile.PrivateFileMode); err != nil {
		return distribution.Release{}, err
	}
	manifest := distribution.Manifest{
		SchemaVersion: distribution.ManifestSchemaVersion,
		Package:       previewPackageName,
		Version:       previewPackageVersion,
		Artifacts:     []distribution.Artifact{{Path: previewModulePath}, {Path: themeResourcePath}},
		Contracts:     []framehost.ContractRef{contracts.Previews.Ref(), contracts.Theme.Ref()},
		Units: []distribution.Unit{
			{ID: previewUnitID, Profile: framehost.BrowserJS, Exports: []framehost.ExportDeclaration{{Name: previewExportName, Contract: contracts.Previews.Ref()}}, Module: &distribution.Module{Entry: previewModulePath}},
			{ID: themeUnitID, Profile: framehost.StaticResource, Exports: []framehost.ExportDeclaration{{Name: themeExportName, Contract: contracts.Theme.Ref()}}, Resources: []string{themeResourcePath}},
		},
	}
	var archive bytes.Buffer
	if _, err := distribution.Build(ctx, directory, manifest, &archive); err != nil {
		return distribution.Release{}, err
	}
	path := filepath.Join(directory, "preview.zip")
	if err := os.WriteFile(path, archive.Bytes(), localfile.PrivateFileMode); err != nil {
		return distribution.Release{}, err
	}
	authorize(value.Hash(archive.Bytes()))
	return store.ImportArchive(ctx, path, value.Hash(archive.Bytes()))
}

func previewDefinition(release distribution.Release) (*framehost.Plugin, error) {
	units := make([]framehost.Unit, 0, len(release.Manifest.Units))
	for _, unit := range release.Manifest.Units {
		if len(unit.Imports) != 0 {
			return nil, errors.New("document: built-in preview has no imports")
		}
		units = append(units, framehost.Unit{Declaration: framehost.UnitDeclaration{ID: unit.ID, Profile: unit.Profile, Exports: unit.Exports}})
	}
	return framehost.Define(framehost.Definition{Revision: release.Digest, Units: units})
}

type moduleSource struct{ store distribution.Store }

func (s moduleSource) Module(ctx context.Context, binding framehost.UnitBinding) (browser.Artifact, error) {
	release, err := s.store.Inspect(ctx, binding.Source.Release)
	if err != nil {
		return browser.Artifact{}, err
	}
	for _, unit := range release.Manifest.Units {
		if unit.ID == binding.Source.UnitID && unit.Profile == framehost.BrowserJS && unit.Module != nil {
			for _, artifact := range release.Manifest.Artifacts {
				if artifact.Path == unit.Module.Entry {
					location := url.URL{Path: artifactPathPrefix + release.Digest.String() + "/" + artifact.Path}
					return browser.Artifact{Path: location.String(), Digest: artifact.Digest}, nil
				}
			}
		}
	}
	return browser.Artifact{}, framehost.ErrUnavailable
}

func (a *App) artifact(ctx context.Context, path string) ([]byte, string, error) {
	release, name, found := strings.Cut(path, "/")
	if !found || release != a.preview.Digest.String() {
		return nil, "", framehost.ErrUnavailable
	}
	lease, err := a.Content.Acquire(ctx, a.preview.Digest)
	if err != nil {
		return nil, "", err
	}
	defer lease.Close()
	data, err := lease.Read(ctx, name)
	if err != nil {
		return nil, "", err
	}
	contentType := "application/octet-stream"
	if strings.HasSuffix(name, ".js") {
		contentType = "text/javascript; charset=utf-8"
	} else if strings.HasSuffix(name, ".css") {
		contentType = "text/css; charset=utf-8"
	}
	return data, contentType, nil
}
