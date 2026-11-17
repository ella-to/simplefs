//go:build !unix

package dirfs

import "io/fs"

// sysIDs is unknown on this platform.
func sysIDs(fs.FileInfo) (uint64, uint32) { return 0, 0 }
