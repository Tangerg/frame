//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package runner

func socketPathLimit() int { return 0 }
