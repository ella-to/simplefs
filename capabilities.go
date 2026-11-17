package simplefs

import (
	"context"
	"time"
)

// Optional capabilities.
//
// A provider advertises a capability by implementing the interface; backends
// discover them with As, which also looks through middleware:
//
//	if sp, ok := simplefs.As[simplefs.SymlinkProvider](provider); ok { ... }
//
// A capability that is implemented but cannot serve a particular call should
// return ErrNotSupported.

// SymlinkProvider supports symbolic links.
type SymlinkProvider interface {
	Readlink(ctx context.Context, path string) (target string, err error)
	Symlink(ctx context.Context, target, linkPath string) error
}

// HardLinkProvider supports hard links.
type HardLinkProvider interface {
	Link(ctx context.Context, existingPath, newPath string) error
}

// ChmodProvider supports changing permission bits.
type ChmodProvider interface {
	Chmod(ctx context.Context, path string, mode FileMode) error
}

// ChownProvider supports changing ownership.
//
// On Windows this may simply return ErrNotSupported.
type ChownProvider interface {
	Chown(ctx context.Context, path string, uid, gid int) error
}

// ChtimesProvider supports changing file timestamps.
//
// As with os.Chtimes, a zero time.Time leaves the corresponding timestamp
// unchanged, so callers can set only the access or only the modification time.
type ChtimesProvider interface {
	Chtimes(ctx context.Context, path string, atime, mtime time.Time) error
}

// XattrProvider supports extended attributes.
//
// Linux/macOS map naturally to xattrs. Windows may emulate these with
// alternate data streams or metadata.
type XattrProvider interface {
	GetXattr(ctx context.Context, path, name string) ([]byte, error)
	SetXattr(ctx context.Context, path, name string, value []byte) error
	ListXattr(ctx context.Context, path string) ([]string, error)
	RemoveXattr(ctx context.Context, path, name string) error
}

// StatFSProvider supports filesystem statistics.
//
// Equivalent to statfs().
type StatFSProvider interface {
	StatFS(ctx context.Context) (FSInfo, error)
}

// WatchProvider supports watching filesystem changes.
//
// Watch reports changes to path itself and its direct children, or with
// recursive to every descendant. The returned channel is closed when ctx is
// cancelled; cancel the context to release the watch. Delivery must never
// block the provider: a receiver that falls behind may miss events.
type WatchProvider interface {
	Watch(ctx context.Context, path string, recursive bool) (<-chan Event, error)
}

// FSInfo describes overall filesystem capacity. Counts are in blocks of
// BlockSize bytes, and Available <= Free <= Blocks.
type FSInfo struct {
	BlockSize uint64 // bytes per block
	Blocks    uint64 // total blocks
	Free      uint64 // free blocks
	Available uint64 // free blocks usable by unprivileged users

	MaxNameLength uint32 // longest path element, in bytes; 0 = unknown
	ReadOnly      bool   // all mutations fail with ErrReadOnly
}

// EventType identifies a filesystem event. The zero value is unknown.
type EventType uint8

// The kinds of change a WatchProvider reports.
const (
	EventCreate EventType = iota + 1
	EventDelete
	EventModify
	EventRename
)

// String returns "create", "delete", "modify", "rename", or "unknown".
func (t EventType) String() string {
	switch t {
	case EventCreate:
		return "create"
	case EventDelete:
		return "delete"
	case EventModify:
		return "modify"
	case EventRename:
		return "rename"
	default:
		return "unknown"
	}
}

// Event is emitted by WatchProvider.
type Event struct {
	// Type is the kind of change.
	Type EventType

	// Path is the affected path (the destination, for a rename).
	Path string

	// OldPath is the source path of a rename; empty otherwise.
	OldPath string

	// Info is the object's metadata after the change, when available.
	Info *FileInfo
}
