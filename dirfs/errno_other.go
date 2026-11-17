//go:build !unix && !windows

package dirfs

import (
	"syscall"
)

func errnoSentinel(syscall.Errno) error { return nil }
