package simplefs

import (
	"context"
	"time"
)

// Optional Capabilities
//
// A provider advertises a capability by implementing the interface; mounts
// discover them with type assertions:
//
//	if sp, ok := provider.(fs.SymlinkProvider); ok { ... }
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
// The returned channel is closed when ctx is cancelled; cancel the context to
// release the watch.
type WatchProvider interface {
	Watch(ctx context.Context, path string, recursive bool) (<-chan Event, error)
}

// FSInfo describes overall filesystem capacity.
type FSInfo struct {
	BlockSize uint64
	Blocks    uint64
	Free      uint64
	Available uint64

	MaxNameLength uint32
	ReadOnly      bool
}

// EventType identifies a filesystem event.
type EventType uint32

const (
	EventCreate EventType = 1 << iota
	EventDelete
	EventModify
	EventRename
)

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
	Type EventType

	Path string

	// Used for rename operations.
	OldPath string

	Info *FileInfo
}
