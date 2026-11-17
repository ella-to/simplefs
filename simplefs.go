// Package simplefs defines a platform-neutral filesystem provider interface.
//
// A backend (S3, Dropbox, a database, an in-memory tree, ...) implements
// FileProvider — and optionally any of the capability interfaces in
// capabilities.go — using plain Go types. A mount implementation (macOS File
// Provider, FUSE, WinFsp, ...) consumes the same interface and translates it
// into whatever the host operating system needs. Neither side depends on the
// other's platform details.
//
// All paths are logical: '/' separated, rooted at "/", never containing "." or
// ".." segments. Use CleanPath to normalize untrusted input. Providers should
// return errors that wrap the sentinel errors in errors.go (ErrNotExist,
// ErrExist, ...) — mounts rely on errors.Is to map failures onto native error
// codes.
package simplefs

import (
	"context"
	"io"
	"io/fs"
	"time"
)

// FileType describes the type of an entry. The zero value is unknown.
//
// This intentionally avoids exposing platform-specific concepts. The backend
// (NFS server, File Provider, ...) is responsible for translating these into
// native file types.
type FileType uint8

// The supported file types.
const (
	TypeFile FileType = iota + 1
	TypeDir
	TypeSymlink
)

// IsFile reports whether the type is a regular file.
func (t FileType) IsFile() bool { return t == TypeFile }

// IsDir reports whether the type is a directory.
func (t FileType) IsDir() bool { return t == TypeDir }

// IsSymlink reports whether the type is a symbolic link.
func (t FileType) IsSymlink() bool { return t == TypeSymlink }

// String returns "file", "dir", "symlink", or "unknown".
func (t FileType) String() string {
	switch t {
	case TypeFile:
		return "file"
	case TypeDir:
		return "dir"
	case TypeSymlink:
		return "symlink"
	default:
		return "unknown"
	}
}

// FileMode holds permission bits (e.g. 0o644). It is io/fs.FileMode, but a
// FileInfo.Mode carries only the permission bits: the type lives in
// FileInfo.Type.
type FileMode = fs.FileMode

// FileInfo describes a single filesystem object.
//
// Unlike io/fs.FileInfo, this is a plain struct instead of an interface,
// which makes it easy to serialize over RPC and to cache.
type FileInfo struct {
	// Path is the full logical path, in CleanPath form. Providers must set
	// it.
	Path string

	// Type is file, directory, or symlink.
	Type FileType

	// Size in bytes. For a symlink, the length of its target.
	Size int64

	// Mode holds the permission bits only.
	Mode FileMode

	// ModTime is the last modification time.
	ModTime time.Time

	// ETag is an optional opaque version identifier.
	//
	// Useful for remote filesystems (S3, Dropbox, Google Drive, git,
	// databases, etc.) for optimistic concurrency, and used by backends to
	// decide when cached content is stale. When non-empty it must change
	// whenever the object's content or metadata changes.
	ETag string
}

// Name returns the base name of the object (BaseName of Path).
func (fi FileInfo) Name() string { return BaseName(fi.Path) }

// IsDir reports whether the info describes a directory.
func (fi FileInfo) IsDir() bool { return fi.Type.IsDir() }

// DirEntry represents one item returned from ReadDir.
//
// Similar to io/fs.DirEntry, but easier to serialize and cache.
type DirEntry struct {
	Name string
	Type FileType

	// Optional metadata.
	//
	// Providers may omit this if obtaining metadata is expensive; consumers
	// fall back to Stat.
	Info *FileInfo
}

// IsDir reports whether the entry is a directory.
func (e DirEntry) IsDir() bool { return e.Type.IsDir() }

// Page requests a page of directory entries.
//
// Token is provider-specific and should be treated as opaque. A zero Page
// (Limit <= 0, empty Token) requests everything in one page.
type Page struct {
	Limit int
	Token string
}

// DirPage contains one page of directory entries.
type DirPage struct {
	Entries []DirEntry

	// Empty when no additional pages exist. Pass it back as Page.Token to
	// fetch the next page.
	NextToken string
}

// OpenOptions control how a file is opened.
type OpenOptions struct {
	Read      bool
	Write     bool
	Create    bool
	Truncate  bool
	Append    bool
	Exclusive bool
}

// CreateOptions customize Create().
type CreateOptions struct {
	Mode FileMode
}

// MkdirOptions customize Mkdir().
type MkdirOptions struct {
	Mode FileMode
}

// RemoveOptions customize Remove().
type RemoveOptions struct {
	// Allow deleting directories recursively.
	Recursive bool

	// Ignore missing files.
	Force bool
}

// RenameOptions customize Rename().
type RenameOptions struct {
	// Replace destination if it already exists.
	Overwrite bool
}

// FileHandle represents an opened file.
//
// Inspired by *os.File but intentionally much smaller. Random access is
// preferred because mounts frequently issue concurrent reads and writes at
// arbitrary offsets.
type FileHandle interface {
	io.ReaderAt
	io.WriterAt
	io.Closer

	// Return the latest metadata.
	Stat(ctx context.Context) (FileInfo, error)

	// Flush pending writes.
	Sync(ctx context.Context) error

	// Resize the file.
	Truncate(ctx context.Context, size int64) error
}

// FileProvider is the minimum interface required to expose a hierarchical
// filesystem.
//
// Everything else (symlinks, xattrs, watching, statfs, etc.) should be
// implemented as optional capability interfaces (see capabilities.go).
type FileProvider interface {

	// Return metadata for a file or directory.
	Stat(ctx context.Context, path string) (FileInfo, error)

	// Open an existing file (or create one, when opts.Create is set).
	Open(ctx context.Context, path string, opts OpenOptions) (FileHandle, error)

	// Read directory contents.
	//
	// Providers may paginate results for very large directories.
	ReadDir(ctx context.Context, path string, page Page) (DirPage, error)

	// Create a new file, truncating it if it already exists.
	Create(ctx context.Context, path string, opts CreateOptions) (FileHandle, error)

	// Create a directory.
	Mkdir(ctx context.Context, path string, opts MkdirOptions) error

	// Remove a file or directory.
	Remove(ctx context.Context, path string, opts RemoveOptions) error

	// Move or rename a file or directory.
	Rename(ctx context.Context, oldPath, newPath string, opts RenameOptions) error
}
