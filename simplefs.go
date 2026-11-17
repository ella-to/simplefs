// Package simplefs defines a platform-neutral filesystem provider interface.
//
// A provider (S3, Dropbox, a database, an in-memory tree, ...) implements
// FileProvider — and optionally any of the capability interfaces in
// capabilities.go — using plain Go types. A backend (the nfs server, the
// macOS File Provider bridge, the mount package) consumes the same interface
// and translates it into whatever the host operating system needs. Neither
// side depends on the other's platform details.
//
// Helpers (ReadFile, WriteFile, MkdirAll, Entries, WalkDir, Copy, ...) cover
// common tasks on any provider, FS adapts a provider to io/fs, FromFS adapts
// an io/fs.FS to a provider, and ReadOnly and Sub wrap providers.
//
// All paths are logical: '/' separated, rooted at "/", never containing "." or
// ".." segments. Use CleanPath to normalize untrusted input. Providers should
// return errors that wrap the sentinel errors in errors.go (ErrNotExist,
// ErrExist, ...) — backends rely on errors.Is to map failures onto native
// error codes.
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
	// Name is the entry's base name.
	Name string

	// Type is the entry's type (symlinks are reported as links).
	Type FileType

	// Info is optional metadata. Providers should fill it when it is cheap
	// to obtain — backends otherwise issue one Stat per entry.
	Info *FileInfo
}

// IsDir reports whether the entry is a directory.
func (e DirEntry) IsDir() bool { return e.Type.IsDir() }

// Page requests a page of directory entries.
//
// A zero Page (Limit <= 0, empty Token) requests everything in one page.
type Page struct {
	// Limit is the maximum number of entries to return; <= 0 means all.
	Limit int

	// Token is the NextToken of the previous page; empty for the first.
	// It is provider-specific and opaque to callers.
	Token string
}

// DirPage contains one page of directory entries.
type DirPage struct {
	// Entries holds at most Page.Limit entries (when a limit was given).
	Entries []DirEntry

	// NextToken is empty when no additional pages exist. Pass it back as
	// Page.Token to fetch the next page.
	NextToken string
}

// OpenOptions control how a file is opened. They mirror the os.O_* flags.
type OpenOptions struct {
	// Read and Write select the access mode. Neither set means Read.
	Read  bool
	Write bool

	// Create creates the file if it does not exist.
	Create bool

	// Truncate empties an existing file. Requires Write.
	Truncate bool

	// Append makes every WriteAt write at the end of the file, ignoring
	// its offset. Requires Write.
	Append bool

	// Exclusive, with Create, fails with ErrExist if the file exists.
	Exclusive bool

	// Mode is the permission bits of a file Create creates; zero means
	// 0o644. It is ignored when the file already exists.
	Mode FileMode
}

// MkdirOptions customize Mkdir.
type MkdirOptions struct {
	// Mode is the new directory's permission bits; zero means 0o755.
	Mode FileMode
}

// RemoveOptions customize Remove.
type RemoveOptions struct {
	// Recursive allows removing a non-empty directory and its contents.
	// Providers that cannot do this return ErrNotSupported; the RemoveAll
	// helper then removes the tree bottom-up.
	Recursive bool

	// Force turns a missing path into success.
	Force bool
}

// RenameOptions customize Rename.
type RenameOptions struct {
	// Overwrite replaces an existing destination.
	Overwrite bool
}

// FileHandle represents an opened file.
//
// Inspired by *os.File but intentionally much smaller. Random access is
// preferred because backends issue concurrent reads and writes at arbitrary
// offsets. All methods must be safe for concurrent use.
//
// Contract:
//   - ReadAt at or past the end returns 0, io.EOF; a short read returns
//     n, io.EOF. A negative offset is ErrInvalid.
//   - WriteAt past the end zero-fills the gap. On a handle opened with
//     Append the offset is ignored and data goes to the end. Writing a
//     handle opened without Write is ErrPermission.
//   - Truncate that grows zero-fills; that shrinks discards. It requires
//     Write.
//   - The handle stays usable after its path is renamed (Stat then reports
//     the new path). After the path is removed it may keep working (POSIX
//     style) or fail with ErrNotExist.
//   - Any use after Close, including a second Close, is ErrClosed.
type FileHandle interface {
	io.ReaderAt
	io.WriterAt
	io.Closer

	// Stat returns the file's current metadata.
	Stat(ctx context.Context) (FileInfo, error)

	// Sync makes previous writes durable.
	Sync(ctx context.Context) error

	// Truncate changes the file's size.
	Truncate(ctx context.Context, size int64) error
}

// FileProvider is the minimum interface required to expose a hierarchical
// filesystem. Everything else (symlinks, xattrs, watching, statfs, ...) is an
// optional capability interface; see capabilities.go and As.
//
// All methods must be safe for concurrent use. The conformance suite in
// package providertest checks the contract below.
//
// Paths: every method accepts any '/'-separated path and must behave as if
// it had been passed through CleanPath (so ".." can never escape the root).
// Every FileInfo.Path returned is in CleanPath form. "/" always exists, is a
// directory, and can be neither removed nor renamed (ErrInvalid). Names are
// byte strings: Unicode is preserved as given (NFC and NFD forms are
// different names); a name longer than the provider's limit is
// ErrNameTooLong.
//
// Errors: wrap the sentinels in errors.go, ideally in a *PathError. Missing
// paths are ErrNotExist; a path whose parent component is a file is
// ErrNotDir. Context cancellation should be returned as an error wrapping
// ctx.Err().
type FileProvider interface {
	// Stat returns metadata for path without following a final symlink.
	Stat(ctx context.Context, path string) (FileInfo, error)

	// Open opens the file at path, or creates it when opts.Create is set
	// (the parent must exist: ErrNotExist otherwise). Opening a directory
	// is ErrIsDir; Truncate or Append without Write is ErrInvalid; Create
	// with Exclusive on an existing file is ErrExist. The Create helper is
	// shorthand for Open{Read, Write, Create, Truncate}.
	Open(ctx context.Context, path string, opts OpenOptions) (FileHandle, error)

	// ReadDir lists the directory at path, never including "." or "..".
	// Listing a file is ErrNotDir. Pages of a directory that does not change
	// between calls contain each entry exactly once; a provider should
	// still return (not loop or panic) when it changes. The Entries helper
	// drains the pages.
	ReadDir(ctx context.Context, path string, page Page) (DirPage, error)

	// Mkdir creates a directory. An existing path is ErrExist; a missing
	// parent is ErrNotExist.
	Mkdir(ctx context.Context, path string, opts MkdirOptions) error

	// Remove removes a file, symlink, or directory. A missing path is
	// ErrNotExist (nil with Force); a non-empty directory is ErrNotEmpty
	// unless Recursive.
	Remove(ctx context.Context, path string, opts RemoveOptions) error

	// Rename moves oldPath to newPath, keeping a directory's whole subtree.
	// Renaming a path onto itself is a no-op. An existing destination is
	// ErrExist unless Overwrite; even then a file cannot replace a
	// directory (ErrIsDir), a directory cannot replace a file (ErrNotDir)
	// or a non-empty directory (ErrNotEmpty). Moving a directory into its
	// own subtree is ErrInvalid. Overwriting should be atomic for readers.
	Rename(ctx context.Context, oldPath, newPath string, opts RenameOptions) error
}
