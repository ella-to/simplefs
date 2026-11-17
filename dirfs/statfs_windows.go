//go:build windows

package dirfs

import (
	"syscall"
	"unsafe"

	"ella.to/simplefs"
)

var procGetDiskFreeSpaceExW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

func statFS(dir string) (simplefs.FSInfo, error) {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return simplefs.FSInfo{}, err
	}
	var avail, total, free uint64
	r, _, e := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&avail)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&free)),
	)
	if r == 0 {
		return simplefs.FSInfo{}, e
	}
	const bs = 4096
	return simplefs.FSInfo{
		BlockSize:     bs,
		Blocks:        total / bs,
		Free:          free / bs,
		Available:     min(avail, free) / bs,
		MaxNameLength: simplefs.MaxNameLength,
	}, nil
}
