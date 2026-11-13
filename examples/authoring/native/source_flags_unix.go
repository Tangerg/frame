//go:build unix

package native

import (
	"syscall"
)

// A path may become a FIFO before open; inspect its descriptor without waiting for a writer.
const sourceOpenFlags = syscall.O_NONBLOCK
