package localfile

import "io/fs"

// State that authorizes or proves work is readable only by the OS user that
// owns it; Frame's trust boundary is that same local user.
const (
	PrivateDirectoryMode = fs.FileMode(0o700)
	PrivateFileMode      = fs.FileMode(0o600)
)

// Generated sources are ordinary files of the consumer's checkout.
const (
	PublicDirectoryMode = fs.FileMode(0o755)
	PublicFileMode      = fs.FileMode(0o644)
)
