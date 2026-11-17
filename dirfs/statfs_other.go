//go:build !(linux || darwin || freebsd || dragonfly || windows)

package dirfs

import "ella.to/simplefs"

func statFS(string) (simplefs.FSInfo, error) { return simplefs.FSInfo{}, simplefs.ErrNotSupported }
