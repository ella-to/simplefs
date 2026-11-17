//go:build unix

package dirfs

import (
	"syscall"

	"ella.to/simplefs"
)

func errnoSentinel(e syscall.Errno) error {
	switch e {
	case syscall.ENOTDIR:
		return simplefs.ErrNotDir
	case syscall.EISDIR:
		return simplefs.ErrIsDir
	case syscall.ENOTEMPTY:
		return simplefs.ErrNotEmpty
	case syscall.EROFS:
		return simplefs.ErrReadOnly
	case syscall.ENOSPC, syscall.EDQUOT:
		return simplefs.ErrNoSpace
	case syscall.EFBIG:
		return simplefs.ErrTooLarge
	case syscall.ENAMETOOLONG:
		return simplefs.ErrNameTooLong
	case syscall.EINVAL:
		return simplefs.ErrInvalid
	case syscall.ENOTSUP, syscall.EXDEV:
		return simplefs.ErrNotSupported
	case syscall.EBUSY, syscall.EAGAIN:
		return simplefs.ErrBusy
	}
	return nil
}
