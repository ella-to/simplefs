package fs

import (
	"context"
	"io"
	"time"
)

////////////////////////////////////////////////////////////////////////////////
// Core Types
////////////////////////////////////////////////////////////////////////////////

// FileType describes the type of an entry.
//
// This intentionally avoids exposing platform-specific concepts.
// The mount implementation (FUSE, WinFsp, macFUSE, etc.) is responsible
// for translating these into native file types.
type FileType uint32

const (
	TypeFile FileType = 1 << iota
	TypeDirectory
	TypeSymlink
)

// FileMode is intentionally similar to os.FileMode, but you may decide
// to alias os.FileMode instead if you don't mind depending on os.
type FileMode uint32

// FileInfo describes a single filesystem object.
//
// Unlike os.FileInfo, this is a plain struct instead of an interface,
// making it easier to serialize over RPC and cache.
type FileInfo struct {
	// Full logical path relative to the filesystem root.
	// Always uses '/' as separator.
	Path string

	// Base name.
	Name string

	// File, Directory, Symlink...
	Type FileType

	// Size in bytes.
	Size int64

	// Permission bits.
	Mode FileMode

	// Last modification time.
	ModTime time.Time

	// Optional opaque version identifier.
	//
	// Useful for remote filesystems (S3, Dropbox, Google Drive,
	// git, databases, etc.) for optimistic concurrency.
	ETag string
}

// DirEntry represents one item returned from ReadDir.
//
// Similar to io/fs.DirEntry, but easier to serialize and cache.
type DirEntry struct {
	Name string
	Type FileType

	// Optional metadata.
	//
	// Providers may omit this if obtaining metadata is expensive.
	Info *FileInfo
}

////////////////////////////////////////////////////////////////////////////////
// Pagination
////////////////////////////////////////////////////////////////////////////////

// Page requests a page of directory entries.
//
// Token is provider-specific and should be treated as opaque.
type Page struct {
	Limit int
	Token string
}

// DirPage contains one page of directory entries.
type DirPage struct {
	Entries []DirEntry

	// Empty when no additional pages exist.
	NextToken string
}

////////////////////////////////////////////////////////////////////////////////
// Options
////////////////////////////////////////////////////////////////////////////////

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

////////////////////////////////////////////////////////////////////////////////
// File Handle
////////////////////////////////////////////////////////////////////////////////

// FileHandle represents an opened file.
//
// Inspired by *os.File but intentionally much smaller.
// Random access is preferred because FUSE frequently issues
// concurrent reads and writes at arbitrary offsets.
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

////////////////////////////////////////////////////////////////////////////////
// Core Provider
////////////////////////////////////////////////////////////////////////////////

// FileProvider is the minimum interface required to expose
// a hierarchical filesystem.
//
// Everything else (symlinks, xattrs, watching, ACLs, etc.)
// should be implemented as optional capability interfaces.
type FileProvider interface {

	// Return metadata for a file or directory.
	Stat(ctx context.Context, path string) (FileInfo, error)

	// Open an existing file.
	Open(ctx context.Context, path string, opts OpenOptions) (FileHandle, error)

	// Read directory contents.
	//
	// Providers may paginate results for very large directories.
	ReadDir(ctx context.Context, path string, page Page) (DirPage, error)

	// Create a new file.
	Create(ctx context.Context, path string, opts CreateOptions) (FileHandle, error)

	// Create a directory.
	Mkdir(ctx context.Context, path string, opts MkdirOptions) error

	// Remove a file or directory.
	Remove(ctx context.Context, path string, opts RemoveOptions) error

	// Move or rename a file or directory.
	Rename(ctx context.Context, oldPath, newPath string, opts RenameOptions) error
}

////////////////////////////////////////////////////////////////////////////////
// Optional Capabilities
////////////////////////////////////////////////////////////////////////////////

// Supports symbolic links.
type SymlinkProvider interface {
	Readlink(ctx context.Context, path string) (target string, err error)
	Symlink(ctx context.Context, target, linkPath string) error
}

// Supports hard links.
type HardLinkProvider interface {
	Link(ctx context.Context, existingPath, newPath string) error
}

// Supports chmod.
type ChmodProvider interface {
	Chmod(ctx context.Context, path string, mode FileMode) error
}

// Supports chown.
//
// On Windows this may simply return ErrNotSupported.
type ChownProvider interface {
	Chown(ctx context.Context, path string, uid, gid int) error
}

// Supports file timestamps.
type ChtimesProvider interface {
	Chtimes(ctx context.Context, path string, atime, mtime time.Time) error
}

// Supports extended attributes.
//
// Linux/macOS map naturally to xattrs.
// Windows may emulate these with alternate data streams or metadata.
type XattrProvider interface {
	GetXattr(ctx context.Context, path, name string) ([]byte, error)
	SetXattr(ctx context.Context, path, name string, value []byte) error
	ListXattr(ctx context.Context, path string) ([]string, error)
	RemoveXattr(ctx context.Context, path, name string) error
}

// Supports filesystem statistics.
//
// Equivalent to statfs().
type StatFSProvider interface {
	StatFS(ctx context.Context) (FSInfo, error)
}

// Supports watching filesystem changes.
type WatchProvider interface {
	Watch(ctx context.Context, path string, recursive bool) (<-chan Event, error)
}

////////////////////////////////////////////////////////////////////////////////
// Filesystem Information
////////////////////////////////////////////////////////////////////////////////

// FSInfo describes overall filesystem capacity.
type FSInfo struct {
	BlockSize uint64
	Blocks    uint64
	Free      uint64
	Available uint64

	MaxNameLength uint32
	ReadOnly      bool
}

////////////////////////////////////////////////////////////////////////////////
// Events
////////////////////////////////////////////////////////////////////////////////

// EventType identifies a filesystem event.
type EventType uint32

const (
	EventCreate EventType = 1 << iota
	EventDelete
	EventModify
	EventRename
)

// Event is emitted by WatchProvider.
type Event struct {
	Type EventType

	Path string

	// Used for rename operations.
	OldPath string

	Info *FileInfo
}