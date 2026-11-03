package distribution

import "io/fs"

const (
	privateDirectoryMode = fs.FileMode(0o700)
	sealedDirectoryMode  = fs.FileMode(0o500)
	privateFileMode      = fs.FileMode(0o600)
	sealedFileMode       = fs.FileMode(0o400)
	ownerExecuteBit      = fs.FileMode(0o100)
	fileExecuteMask      = fs.FileMode(0o111)
)
