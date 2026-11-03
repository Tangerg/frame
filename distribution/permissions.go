package distribution

import (
	"io/fs"

	"github.com/Tangerg/frame/internal/localfile"
)

const (
	privateDirectoryMode = localfile.PrivateDirectoryMode
	sealedDirectoryMode  = fs.FileMode(0o500)
	privateFileMode      = localfile.PrivateFileMode
	sealedFileMode       = fs.FileMode(0o400)
	ownerExecuteBit      = fs.FileMode(0o100)
	fileExecuteMask      = fs.FileMode(0o111)
)
