//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package runner

import "syscall"

// Pathname sockets reserve one terminating NUL in the platform sockaddr.
func socketPathLimit() int { return len(syscall.RawSockaddrUnix{}.Path) - 1 }
