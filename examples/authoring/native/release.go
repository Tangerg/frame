package native

import (
	"io/fs"

	framevalue "github.com/Tangerg/frame/value"
)

func pluginRevision() (framevalue.Digest, error) {
	type sourceFile struct {
		Path   string `json:"path"`
		Source string `json:"source"`
	}
	entries, err := fs.ReadDir(pluginSources, ".")
	if err != nil {
		return framevalue.Digest{}, err
	}
	sources := make([]sourceFile, 0, len(entries))
	for _, entry := range entries {
		content, err := pluginSources.ReadFile(entry.Name())
		if err != nil {
			return framevalue.Digest{}, err
		}
		sources = append(sources, sourceFile{Path: entry.Name(), Source: string(content)})
	}
	encoded, err := framevalue.EncodeJSON(sources)
	if err != nil {
		return framevalue.Digest{}, err
	}
	return framevalue.Hash(encoded.Bytes()), nil
}
