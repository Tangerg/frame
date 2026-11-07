package distribution

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/Tangerg/frame/internal/cancellation"
)

func (paths pathInventory) walk(ctx context.Context, root *os.Root, limit int, visit func(string, fs.DirEntry) error) error {
	identity, err := root.Stat(".")
	if err != nil {
		return err
	}
	return paths.walkDirectory(ctx, root, ".", identity, limit, visit)
}

func (paths pathInventory) walkDirectory(ctx context.Context, root *os.Root, name string, identity fs.FileInfo, limit int, visit func(string, fs.DirEntry) error) error {
	entries, err := paths.readDirectory(ctx, root, name, identity, limit)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := cancellation.Err(ctx); err != nil {
			return err
		}
		child := path.Join(name, entry.Name())
		if err := visit(child, entry); err != nil {
			return err
		}
		if entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if err := paths.walkDirectory(ctx, root, child, info, limit, visit); err != nil {
				return err
			}
		}
	}
	return nil
}

func (paths pathInventory) readDirectory(ctx context.Context, root *os.Root, name string, identity fs.FileInfo, limit int) (_ []fs.DirEntry, err error) {
	if err := cancellation.Err(ctx); err != nil {
		return nil, err
	}
	directory, err := root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	opened, err := directory.Stat(".")
	if err != nil || !os.SameFile(identity, opened) || opened.Mode() != identity.Mode() {
		return nil, errors.Join(ErrIntegrity, err)
	}
	file, err := directory.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	return paths.readEntries(ctx, file, name, limit)
}

// The inventory admits entries before another batch can be allocated. Only
// admitted entries are sorted; WalkDir would read an unbounded directory first.
func (paths pathInventory) readEntries(ctx context.Context, directory fs.ReadDirFile, parent string, limit int) ([]fs.DirEntry, error) {
	var admitted []fs.DirEntry
	for {
		if err := cancellation.Err(ctx); err != nil {
			return nil, err
		}
		batchSize := max(1, min(directoryReadBatch, limit-len(paths)))
		entries, readErr := directory.ReadDir(batchSize)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, readErr
		}
		for _, entry := range entries {
			kind := fileEntry
			if entry.IsDir() {
				kind = directoryEntry
			}
			if err := paths.admitBounded(path.Join(parent, entry.Name()), kind, limit); err != nil {
				return nil, err
			}
			admitted = append(admitted, entry)
		}
		if errors.Is(readErr, io.EOF) {
			slices.SortFunc(admitted, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
			return admitted, nil
		}
	}
}
