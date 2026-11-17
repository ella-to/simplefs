//go:build linux || darwin || freebsd || dragonfly

package dirfs

import (
	"syscall"

	"ella.to/simplefs"
)

func statFS(dir string) (simplefs.FSInfo, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return simplefs.FSInfo{}, err
	}
	info := simplefs.FSInfo{
		BlockSize:     nonNeg(st.Bsize),
		Blocks:        nonNeg(st.Blocks),
		Free:          nonNeg(st.Bfree),
		Available:     nonNeg(st.Bavail),
		MaxNameLength: simplefs.MaxNameLength,
	}
	info.Available = min(info.Available, info.Free)
	info.Free = min(info.Free, info.Blocks)
	return info, nil
}

// nonNeg converts the per-OS Statfs_t field types, clamping negatives
// (FreeBSD reports Bavail as int64 and may go below zero for root reserve).
func nonNeg[T ~int32 | ~int64 | ~uint32 | ~uint64](v T) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}
