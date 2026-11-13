//go:build unix

package localfile

import (
	"syscall"
)

// A FIFO may replace a checked path. Opening must not wait for a writer before fstat.
const nonblocking = syscall.O_NONBLOCK
