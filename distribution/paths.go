package distribution

import (
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
)

// Portable archive paths deliberately use ASCII. This rejects Unicode and
// case-fold aliases rather than depending on a destination filesystem's rules.
const maxArtifactPathBytes = 1024
const maxArtifactPathSegmentBytes = 240

// Release paths must stay valid on every platform a host may extract to, so
// Windows device names and reserved characters are refused everywhere.
var (
	windowsDeviceNames            = []string{"con", "prn", "aux", "nul"}
	windowsNumberedDevicePrefixes = []string{"com", "lpt"}
)

const windowsReservedCharacters = ":*?\"<>|"

func validPath(value string) bool {
	if value == "" || len(value) > maxArtifactPathBytes || !fs.ValidPath(value) || value == "." || strings.Contains(value, "\\") {
		return false
	}
	if strings.ContainsFunc(value, unportableRune) {
		return false
	}
	return !slices.ContainsFunc(strings.Split(value, "/"), unportableSegment)
}

func unportableRune(r rune) bool {
	return r < '!' || r > '~' || strings.ContainsRune(windowsReservedCharacters, r)
}

func unportableSegment(segment string) bool {
	if strings.HasSuffix(segment, ".") || strings.HasSuffix(segment, " ") || len(segment) > maxArtifactPathSegmentBytes {
		return true
	}
	base, _, _ := strings.Cut(segment, ".")
	stem := strings.ToLower(base)
	return slices.Contains(windowsDeviceNames, stem) || numberedDevice(stem)
}

func numberedDevice(stem string) bool {
	if len(stem) != 4 || stem[3] < '1' || stem[3] > '9' {
		return false
	}
	return slices.Contains(windowsNumberedDevicePrefixes, stem[:3])
}

func isReleaseMetadata(name string) bool { return name == ManifestName }

type entryKind uint8

const (
	fileEntry entryKind = iota
	directoryEntry
	ancestorEntry
)

// Implicit ancestors reserve their original spelling before a later explicit
// directory can introduce a filesystem-dependent alias.
type pathEntry struct {
	name string
	kind entryKind
}

type pathInventory map[string]pathEntry

func reservedMetadataPaths() pathInventory {
	return pathInventory{ManifestName: {name: ManifestName, kind: fileEntry}}
}

func (paths pathInventory) ownsFile(name string) bool {
	entry, present := paths[strings.ToLower(name)]
	return present && entry.name == name && entry.kind == fileEntry
}

func (paths pathInventory) admitBounded(name string, kind entryKind, limit int) error {
	if err := paths.admit(name, kind); err != nil {
		return err
	}
	if len(paths) > limit {
		return ErrCapacity
	}
	return nil
}

func (paths pathInventory) admit(name string, kind entryKind) error {
	if !validPath(name) {
		return fmt.Errorf("%w: unsafe path", ErrInvalid)
	}
	key := strings.ToLower(name)
	if prior, exists := paths[key]; exists && (prior.kind != ancestorEntry || kind != directoryEntry || prior.name != name) {
		return fmt.Errorf("%w: duplicate or aliased path", ErrInvalid)
	}
	for ancestor := path.Dir(name); ancestor != "."; ancestor = path.Dir(ancestor) {
		key := strings.ToLower(ancestor)
		if prior, ok := paths[key]; ok {
			if prior.kind == fileEntry || prior.name != ancestor {
				return fmt.Errorf("%w: file or alias is a path ancestor", ErrInvalid)
			}
		} else {
			paths[key] = pathEntry{name: ancestor, kind: ancestorEntry}
		}
	}
	paths[key] = pathEntry{name: name, kind: kind}
	return nil
}
