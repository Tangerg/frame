package distribution

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Tangerg/frame/internal/cancellation"
	"github.com/Tangerg/frame/internal/localfile"
)

const (
	privateDirectoryMode = localfile.PrivateDirectoryMode
	sealedDirectoryMode  = fs.FileMode(0o500)
	privateFileMode      = localfile.PrivateFileMode
	sealedFileMode       = fs.FileMode(0o400)
	ownerExecuteBit      = fs.FileMode(0o100)
	fileExecuteMask      = fs.FileMode(0o111)
	specialFileModes     = fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky
)

type treeAdmission uint8

const (
	unsealedTree treeAdmission = iota
	sealedTree
)

func (a Artifact) fileMode(admission treeAdmission) fs.FileMode {
	mode := privateFileMode
	if admission == sealedTree {
		mode = sealedFileMode
	}
	if a.Executable {
		mode |= ownerExecuteBit
	}
	return mode
}

func sealReleaseContents(ctx context.Context, root *os.Root, manifest Manifest, limit int) error {
	artifacts := make(map[string]Artifact, len(manifest.Artifacts))
	for _, artifact := range manifest.Artifacts {
		artifacts[artifact.Path] = artifact
	}

	type directory struct {
		name     string
		identity fs.FileInfo
	}
	var directories []directory
	paths := pathInventory{}
	err := paths.walk(ctx, root, limit, func(name string, entry fs.DirEntry) error {
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			directories = append(directories, directory{name: name, identity: info})
			return nil
		}
		mode := sealedFileMode
		if artifact, found := artifacts[name]; found {
			mode = artifact.fileMode(sealedTree)
		} else if !isReleaseMetadata(name) {
			return ErrIntegrity
		}
		return sealEntry(root, name, info, mode)
	})
	if err != nil {
		return err
	}
	// A parent entry cannot stand in for durable child entries or final modes.
	// Reverse preorder seals and syncs every directory after all its descendants.
	for index := len(directories) - 1; index >= 0; index-- {
		if err := cancellation.Err(ctx); err != nil {
			return err
		}
		directory := directories[index]
		if err := sealEntry(root, directory.name, directory.identity, sealedDirectoryMode); err != nil {
			return err
		}
	}
	rootFile, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(rootFile.Sync(), rootFile.Close())
}

func sealEntry(root *os.Root, name string, expected fs.FileInfo, mode fs.FileMode) error {
	var file *os.File
	var err error
	if expected.IsDir() {
		file, err = root.Open(name)
	} else {
		file, err = localfile.OpenRegularIn(root, name)
	}
	if err != nil {
		return err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(expected, opened) || opened.Mode().Type() != expected.Mode().Type() {
		return errors.Join(ErrIntegrity, err, file.Close())
	}
	return errors.Join(file.Chmod(mode), file.Sync(), file.Close())
}

func removeSealed(directory string) error {
	if _, err := os.Lstat(directory); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	err := filepath.WalkDir(directory, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(name, privateDirectoryMode)
		}
		return nil
	})
	return errors.Join(err, os.RemoveAll(directory))
}
