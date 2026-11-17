//go:build windows

package dirfs

import (
	"syscall"

	"ella.to/simplefs"
)

// Win32 error codes (winerror.h) that have a simplefs equivalent.
const (
	errWriteProtect     syscall.Errno = 19
	errSharingViolation syscall.Errno = 32
	errLockViolation    syscall.Errno = 33
	errHandleDiskFull   syscall.Errno = 39
	errNotSupported     syscall.Errno = 50
	errDiskFull         syscall.Errno = 112
	errInvalidName      syscall.Errno = 123
	errDirNotEmpty      syscall.Errno = 145
	errFilenameExcRange syscall.Errno = 206
	errDirectoryInvalid syscall.Errno = 267
	errPrivilegeNotHeld syscall.Errno = 1314
)

func errnoSentinel(e syscall.Errno) error {
	switch e {
	case errDirNotEmpty:
		return simplefs.ErrNotEmpty
	case errDirectoryInvalid:
		return simplefs.ErrNotDir
	case errDiskFull, errHandleDiskFull:
		return simplefs.ErrNoSpace
	case errFilenameExcRange:
		return simplefs.ErrNameTooLong
	case errInvalidName:
		return simplefs.ErrInvalid
	case errWriteProtect:
		return simplefs.ErrReadOnly
	case errNotSupported, syscall.EWINDOWS:
		return simplefs.ErrNotSupported
	case errPrivilegeNotHeld:
		return simplefs.ErrPermission
	case errSharingViolation, errLockViolation:
		return simplefs.ErrBusy
	}
	return nil
}
