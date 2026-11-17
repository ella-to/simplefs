package dirfs

import (
	"errors"
	"io/fs"
	"syscall"

	"ella.to/simplefs"
)

// mappedError makes an OS error match a simplefs sentinel with errors.Is,
// while errors.As/Unwrap still reach the original error.
type mappedError struct {
	sentinel error
	err      error
}

func (e *mappedError) Error() string        { return e.err.Error() }
func (e *mappedError) Unwrap() error        { return e.err }
func (e *mappedError) Is(target error) bool { return target == e.sentinel }

// pathErr wraps err (nil stays nil) in a *simplefs.PathError carrying the
// logical path, mapping OS error numbers onto simplefs sentinels.
func pathErr(op, path string, err error) error {
	if err == nil {
		return nil
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		if sentinel := errnoSentinel(errno); sentinel != nil && !errors.Is(err, sentinel) {
			err = &mappedError{sentinel: sentinel, err: err}
		}
	}
	return &simplefs.PathError{Op: op, Path: simplefs.CleanPath(path), Err: err}
}

// isNotEmpty reports whether err says a directory still has entries.
func isNotEmpty(err error) bool {
	var errno syscall.Errno
	return errors.Is(err, simplefs.ErrNotEmpty) ||
		(errors.As(err, &errno) && errnoSentinel(errno) == simplefs.ErrNotEmpty)
}
