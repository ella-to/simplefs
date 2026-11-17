package simplefs

import "errors"

// Sentinel errors providers should wrap (typically inside a *PathError).
// Mounts use errors.Is against these to translate failures into native error
// codes, so returning a bare fmt.Errorf loses information.
var (
	// ErrNotExist indicates the path does not exist.
	ErrNotExist = errors.New("file does not exist")

	// ErrExist indicates the path already exists.
	ErrExist = errors.New("file already exists")

	// ErrIsDir indicates a file operation was attempted on a directory.
	ErrIsDir = errors.New("is a directory")

	// ErrNotDir indicates a directory operation was attempted on a file.
	ErrNotDir = errors.New("not a directory")

	// ErrNotEmpty indicates a non-recursive remove (or an overwriting
	// rename) hit a non-empty directory.
	ErrNotEmpty = errors.New("directory not empty")

	// ErrPermission indicates the operation is not allowed.
	ErrPermission = errors.New("permission denied")

	// ErrNotSupported indicates the provider does not support the operation.
	ErrNotSupported = errors.New("operation not supported")

	// ErrInvalid indicates an invalid argument (bad path, bad offset,
	// conflicting open flags, ...).
	ErrInvalid = errors.New("invalid argument")

	// ErrClosed indicates a FileHandle was used after Close.
	ErrClosed = errors.New("file already closed")

	// ErrNoSpace indicates the provider has no room for the data (disk
	// full, quota exceeded).
	ErrNoSpace = errors.New("no space left on device")
)

// PathError records an error, the operation, and the path that caused it —
// the same shape as io/fs.PathError, without depending on it.
type PathError struct {
	Op   string
	Path string
	Err  error
}

func (e *PathError) Error() string { return e.Op + " " + e.Path + ": " + e.Err.Error() }

func (e *PathError) Unwrap() error { return e.Err }
