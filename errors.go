package simplefs

import (
	"errors"
	"io/fs"
)

// Sentinel errors providers should wrap (typically inside a *PathError).
// Backends use errors.Is against these to translate failures into native
// error codes, so returning a bare fmt.Errorf loses information.
//
// Where the standard library already has a sentinel, these are the very same
// values, so errors.Is(err, fs.ErrNotExist) and errors.Is(err,
// simplefs.ErrNotExist) are interchangeable.
var (
	// ErrNotExist indicates the path does not exist.
	ErrNotExist = fs.ErrNotExist

	// ErrExist indicates the path already exists.
	ErrExist = fs.ErrExist

	// ErrPermission indicates the operation is not allowed.
	ErrPermission = fs.ErrPermission

	// ErrInvalid indicates an invalid argument (bad path, bad offset,
	// conflicting open flags, ...).
	ErrInvalid = fs.ErrInvalid

	// ErrClosed indicates a FileHandle was used after Close.
	ErrClosed = fs.ErrClosed

	// ErrNotSupported indicates the provider does not support the operation.
	ErrNotSupported = errors.ErrUnsupported

	// ErrIsDir indicates a file operation was attempted on a directory.
	ErrIsDir = errors.New("is a directory")

	// ErrNotDir indicates a directory operation was attempted on a file.
	ErrNotDir = errors.New("not a directory")

	// ErrNotEmpty indicates a non-recursive remove (or an overwriting
	// rename) hit a non-empty directory.
	ErrNotEmpty = errors.New("directory not empty")

	// ErrReadOnly indicates a mutation of a read-only filesystem.
	ErrReadOnly = errors.New("read-only file system")

	// ErrNoSpace indicates the provider has no room for the data (disk
	// full, quota exceeded).
	ErrNoSpace = errors.New("no space left on device")

	// ErrTooLarge indicates a file would exceed the provider's maximum
	// file size.
	ErrTooLarge = errors.New("file too large")

	// ErrNameTooLong indicates a path element exceeds the provider's name
	// length limit.
	ErrNameTooLong = errors.New("file name too long")

	// ErrBusy indicates a transient failure (rate limit, backend briefly
	// unavailable); the caller may retry the operation later.
	ErrBusy = errors.New("resource temporarily unavailable")
)

// PathError records an error, the operation, and the path that caused it.
// It is io/fs.PathError, so code written against the standard library
// handles provider errors unchanged.
type PathError = fs.PathError
