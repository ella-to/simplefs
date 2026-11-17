//go:build unix

package dirfs

import (
	"io/fs"
	"syscall"
)

// sysIDs returns the inode number and link count behind fi.
func sysIDs(fi fs.FileInfo) (ino uint64, links uint32) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino), uint32(st.Nlink)
	}
	return 0, 0
}
