// Package memfs is an in-memory implementation of simplefs.FileProvider, intended
// for testing and as a reference implementation. It implements the full core
// interface plus every optional capability: symlinks, hard links, chmod,
// chown, chtimes, xattrs, statfs, and watching.
//
// Everything lives in process memory: create a file through the provider (or
// through a mount backed by it) and it is stored — and lost when the process
// exits.
package memfs

import (
	"context"
	"errors"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ella.to/simplefs"
)

const blockSize = 4096

// node is one object in the tree. Directories own their children by name, so
// nodes themselves are nameless — which is also what lets two directory
// entries share one file node (a hard link).
type node struct {
	typ     simplefs.FileType
	mode    simplefs.FileMode
	modTime time.Time
	uid     int
	gid     int
	xattr   map[string][]byte
	version uint64
	nlink   int    // directory entries referring to this node
	ino     uint64 // FileInfo.FileID; shared by hard links

	data     []byte           // TypeFile
	children map[string]*node // TypeDir
	target   string           // TypeSymlink

	// sorted caches the children's names in order, so paged ReadDir costs
	// O(limit) instead of a sort per page. It is built lazily by readers
	// (possibly concurrently, hence atomic) and reset by every mutation of
	// children, which happens under the write lock.
	sorted atomic.Pointer[[]string]
}

// setChild adds or replaces the entry name. Callers must hold the write lock.
func (n *node) setChild(name string, c *node) {
	n.children[name] = c
	n.sorted.Store(nil)
}

// delChild removes the entry name. Callers must hold the write lock.
func (n *node) delChild(name string) {
	delete(n.children, name)
	n.sorted.Store(nil)
}

// sortedNames returns the children's names in order. Callers must hold at
// least the read lock and must not modify the result.
func (n *node) sortedNames() []string {
	if names := n.sorted.Load(); names != nil {
		return *names
	}
	names := make([]string, 0, len(n.children))
	for name := range n.children {
		names = append(names, name)
	}
	slices.Sort(names)
	n.sorted.Store(&names)
	return names
}

func (n *node) bump() { n.version++ }

func (n *node) size() int64 {
	if n.typ.IsSymlink() {
		return int64(len(n.target))
	}
	return int64(len(n.data))
}

func (n *node) info(path string) simplefs.FileInfo {
	return simplefs.FileInfo{
		Path:    path,
		Type:    n.typ,
		Size:    n.size(),
		Mode:    n.mode,
		ModTime: n.modTime,
		ETag:    strconv.FormatUint(n.version, 10),
		FileID:  n.ino,
		Links:   uint32(max(n.nlink, 1)),
	}
}

type watcher struct {
	path      string
	recursive bool
	ch        chan simplefs.Event
}

// matches reports whether an event at p should be delivered to this watcher:
// the watched path itself, a direct child, or — when recursive — any
// descendant.
func (w *watcher) matches(p string) bool {
	if !simplefs.IsUnder(w.path, p) {
		return false
	}
	return w.recursive || p == w.path || simplefs.ParentPath(p) == w.path
}

// FS is an in-memory simplefs.FileProvider. The zero value is not usable; use
// New. All methods are safe for concurrent use.
type FS struct {
	mu       sync.RWMutex
	root     *node
	watchers map[*watcher]struct{}

	// usedBytes is the total file content of every linked node, kept up to
	// date on each change so StatFS is O(1).
	usedBytes int64
	capacity  int64

	// handles is every open handle, so Rename can keep their paths current.
	handles map[*handle]struct{}

	lastIno uint64 // FileIDs handed out so far
}

var (
	_ simplefs.FileProvider     = (*FS)(nil)
	_ simplefs.SymlinkProvider  = (*FS)(nil)
	_ simplefs.HardLinkProvider = (*FS)(nil)
	_ simplefs.ChmodProvider    = (*FS)(nil)
	_ simplefs.ChownProvider    = (*FS)(nil)
	_ simplefs.ChtimesProvider  = (*FS)(nil)
	_ simplefs.XattrProvider    = (*FS)(nil)
	_ simplefs.StatFSProvider   = (*FS)(nil)
	_ simplefs.WatchProvider    = (*FS)(nil)
)

// DefaultCapacity is the content limit of a filesystem created without
// WithCapacity.
const DefaultCapacity = 1 << 30

// Option configures a FS.
type Option func(*FS)

// WithCapacity limits the total file content to n bytes; writes that would
// exceed it fail with simplefs.ErrNoSpace. StatFS reports this capacity.
func WithCapacity(n int64) Option {
	return func(m *FS) { m.capacity = n }
}

// New returns an empty in-memory filesystem.
func New(opts ...Option) *FS {
	m := &FS{
		root: &node{
			typ:      simplefs.TypeDir,
			mode:     0o755,
			modTime:  time.Now(),
			children: map[string]*node{},
			version:  1,
			nlink:    1,
			ino:      1,
		},
		watchers: map[*watcher]struct{}{},
		handles:  map[*handle]struct{}{},
		capacity: DefaultCapacity,
		lastIno:  1,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// newIno returns a fresh FileID. Callers must hold m.mu for writing.
func (m *FS) newIno() uint64 {
	m.lastIno++
	return m.lastIno
}

// reserveLocked reports whether n's content may grow to size bytes without
// exceeding the capacity. Callers must hold m.mu.
func (m *FS) reserveLocked(n *node, size int64) bool {
	grow := size - int64(len(n.data))
	return grow <= 0 || m.usedBytes+grow <= m.capacity
}

// openHandleLocked registers a new handle. Callers must hold m.mu for writing.
func (m *FS) openHandleLocked(n *node, path string, opts simplefs.OpenOptions) *handle {
	h := &handle{m: m, n: n, path: path, opts: opts}
	m.handles[h] = struct{}{}
	return h
}

// setDataLocked replaces n's content, keeping usedBytes current. Content of
// an unlinked node (still reachable through an open handle) is not counted.
// Callers must hold m.mu for writing.
func (m *FS) setDataLocked(n *node, data []byte) {
	if n.nlink > 0 {
		m.usedBytes += int64(len(data)) - int64(len(n.data))
	}
	n.data = data
}

// linkLocked records one more directory entry referring to n.
func (m *FS) linkLocked(n *node) { n.nlink++ }

// unlinkLocked drops one directory entry referring to n. When the last one
// goes, n's content (and, for a directory, its whole subtree) stops counting
// toward usedBytes. Callers must hold m.mu for writing.
func (m *FS) unlinkLocked(n *node) {
	n.nlink--
	if n.nlink > 0 {
		return
	}
	m.usedBytes -= int64(len(n.data))
	for _, c := range n.children {
		m.unlinkLocked(c)
	}
}

func patherr(op, path string, err error) error {
	return &simplefs.PathError{Op: op, Path: path, Err: err}
}

// walk returns the node at path without following symlinks (Lstat semantics).
// Callers must hold m.mu.
func (m *FS) walk(path string) (*node, error) {
	n := m.root
	p := simplefs.CleanPath(path)
	if p == "/" {
		return n, nil
	}
	for seg := range strings.SplitSeq(p[1:], "/") {
		if !n.typ.IsDir() {
			return nil, simplefs.ErrNotDir
		}
		c, ok := n.children[seg]
		if !ok {
			return nil, simplefs.ErrNotExist
		}
		n = c
	}
	return n, nil
}

// walkParent returns the parent directory node of path and the final path
// segment. Callers must hold m.mu.
func (m *FS) walkParent(path string) (*node, string, error) {
	p := simplefs.CleanPath(path)
	if p == "/" {
		return nil, "", simplefs.ErrInvalid
	}
	parent, err := m.walk(simplefs.ParentPath(p))
	if err != nil {
		return nil, "", err
	}
	if !parent.typ.IsDir() {
		return nil, "", simplefs.ErrNotDir
	}
	name := simplefs.BaseName(p)
	if err := simplefs.ValidName(name); err != nil {
		return nil, "", err
	}
	return parent, name, nil
}

// notify delivers ev to every matching watcher. Callers must hold m.mu (read
// or write); sends never block — a slow watcher drops events.
func (m *FS) notify(ev simplefs.Event) {
	for w := range m.watchers {
		if w.matches(ev.Path) || (ev.OldPath != "" && w.matches(ev.OldPath)) {
			select {
			case w.ch <- ev:
			default:
			}
		}
	}
}

func (m *FS) event(typ simplefs.EventType, path, oldPath string, n *node) {
	var info *simplefs.FileInfo
	if n != nil {
		fi := n.info(path)
		info = &fi
	}
	m.notify(simplefs.Event{Type: typ, Path: path, OldPath: oldPath, Info: info})
}

// Stat returns metadata for the object at path (without following symlinks).
func (m *FS) Stat(ctx context.Context, path string) (simplefs.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return simplefs.FileInfo{}, err
	}
	p := simplefs.CleanPath(path)
	m.mu.RLock()
	defer m.mu.RUnlock()

	n, err := m.walk(p)
	if err != nil {
		return simplefs.FileInfo{}, patherr("stat", p, err)
	}
	return n.info(p), nil
}

// Open opens the file at path. With opts.Create set a missing file is
// created with opts.Mode (0o644 when zero); with opts.Exclusive set as well
// an existing file is an error.
func (m *FS) Open(ctx context.Context, path string, opts simplefs.OpenOptions) (simplefs.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := simplefs.CleanPath(path)
	if !opts.Read && !opts.Write {
		opts.Read = true
	}
	if (opts.Truncate || opts.Append) && !opts.Write {
		return nil, patherr("open", p, simplefs.ErrInvalid)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	n, err := m.walk(p)
	switch {
	case err == nil:
		if opts.Create && opts.Exclusive {
			return nil, patherr("open", p, simplefs.ErrExist)
		}
	case errors.Is(err, simplefs.ErrNotExist) && opts.Create:
		mode := opts.Mode
		if mode == 0 {
			mode = 0o644
		}
		n, err = m.createFileLocked(p, mode)
		if err != nil {
			return nil, patherr("open", p, err)
		}
	default:
		return nil, patherr("open", p, err)
	}

	if n.typ.IsDir() {
		return nil, patherr("open", p, simplefs.ErrIsDir)
	}
	if n.typ.IsSymlink() {
		return nil, patherr("open", p, simplefs.ErrNotSupported)
	}
	if opts.Truncate && len(n.data) > 0 {
		m.setDataLocked(n, nil)
		n.modTime = time.Now()
		n.bump()
		m.event(simplefs.EventModify, p, "", n)
	}
	return m.openHandleLocked(n, p, opts), nil
}

// createFileLocked inserts a new empty file node at p. Callers must hold m.mu
// for writing.
func (m *FS) createFileLocked(p string, mode simplefs.FileMode) (*node, error) {
	parent, name, err := m.walkParent(p)
	if err != nil {
		return nil, err
	}
	n := &node{typ: simplefs.TypeFile, mode: mode, modTime: time.Now(), version: 1, nlink: 1, ino: m.newIno()}
	parent.setChild(name, n)
	parent.modTime = n.modTime
	parent.bump()
	m.event(simplefs.EventCreate, p, "", n)
	return n, nil
}

// ReadDir lists the directory at path. Pagination: entries are sorted by
// name, Page.Token is the last name of the previous page.
func (m *FS) ReadDir(ctx context.Context, path string, page simplefs.Page) (simplefs.DirPage, error) {
	if err := ctx.Err(); err != nil {
		return simplefs.DirPage{}, err
	}
	p := simplefs.CleanPath(path)
	m.mu.RLock()
	defer m.mu.RUnlock()

	n, err := m.walk(p)
	if err != nil {
		return simplefs.DirPage{}, patherr("readdir", p, err)
	}
	if !n.typ.IsDir() {
		return simplefs.DirPage{}, patherr("readdir", p, simplefs.ErrNotDir)
	}

	names := n.sortedNames()
	if page.Token != "" {
		idx, found := slices.BinarySearch(names, page.Token)
		if found {
			idx++
		}
		names = names[idx:]
	}
	limit := page.Limit
	if limit <= 0 || limit > len(names) {
		limit = len(names)
	}

	out := simplefs.DirPage{Entries: make([]simplefs.DirEntry, 0, limit)}
	for _, name := range names[:limit] {
		child := n.children[name]
		info := child.info(simplefs.JoinPath(p, name))
		out.Entries = append(out.Entries, simplefs.DirEntry{Name: name, Type: child.typ, Info: &info})
	}
	if limit < len(names) {
		out.NextToken = names[limit-1]
	}
	return out, nil
}

// Mkdir creates a directory at path. The parent must already exist.
func (m *FS) Mkdir(ctx context.Context, path string, opts simplefs.MkdirOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	mode := opts.Mode
	if mode == 0 {
		mode = 0o755
	}
	p := simplefs.CleanPath(path)

	m.mu.Lock()
	defer m.mu.Unlock()

	parent, name, err := m.walkParent(p)
	if err != nil {
		return patherr("mkdir", p, err)
	}
	if _, ok := parent.children[name]; ok {
		return patherr("mkdir", p, simplefs.ErrExist)
	}
	n := &node{
		typ:      simplefs.TypeDir,
		mode:     mode,
		modTime:  time.Now(),
		children: map[string]*node{},
		version:  1,
		nlink:    1,
		ino:      m.newIno(),
	}
	parent.setChild(name, n)
	parent.modTime = n.modTime
	parent.bump()
	m.event(simplefs.EventCreate, p, "", n)
	return nil
}

// Remove deletes the object at path. Non-empty directories require
// opts.Recursive; opts.Force turns a missing path into a no-op.
func (m *FS) Remove(ctx context.Context, path string, opts simplefs.RemoveOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p := simplefs.CleanPath(path)
	if p == "/" {
		return patherr("remove", p, simplefs.ErrInvalid)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	parent, name, err := m.walkParent(p)
	if err != nil {
		if opts.Force && errors.Is(err, simplefs.ErrNotExist) {
			return nil
		}
		return patherr("remove", p, err)
	}
	n, ok := parent.children[name]
	if !ok {
		if opts.Force {
			return nil
		}
		return patherr("remove", p, simplefs.ErrNotExist)
	}
	if n.typ.IsDir() && len(n.children) > 0 && !opts.Recursive {
		return patherr("remove", p, simplefs.ErrNotEmpty)
	}
	parent.delChild(name)
	m.unlinkLocked(n)
	parent.modTime = time.Now()
	parent.bump()
	m.event(simplefs.EventDelete, p, "", n)
	return nil
}

// Rename moves oldPath to newPath. An existing destination requires
// opts.Overwrite and must be type-compatible (and, for directories, empty).
func (m *FS) Rename(ctx context.Context, oldPath, newPath string, opts simplefs.RenameOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	op, np := simplefs.CleanPath(oldPath), simplefs.CleanPath(newPath)
	if op == "/" || np == "/" {
		return patherr("rename", op, simplefs.ErrInvalid)
	}
	if op == np {
		return nil
	}
	if simplefs.IsUnder(op, np) {
		return patherr("rename", np, simplefs.ErrInvalid) // moving a directory into itself
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	srcParent, srcName, err := m.walkParent(op)
	if err != nil {
		return patherr("rename", op, err)
	}
	src, ok := srcParent.children[srcName]
	if !ok {
		return patherr("rename", op, simplefs.ErrNotExist)
	}
	dstParent, dstName, err := m.walkParent(np)
	if err != nil {
		return patherr("rename", np, err)
	}
	dst, replacing := dstParent.children[dstName]
	if replacing {
		if !opts.Overwrite {
			return patherr("rename", np, simplefs.ErrExist)
		}
		if dst.typ.IsDir() && !src.typ.IsDir() {
			return patherr("rename", np, simplefs.ErrIsDir)
		}
		if !dst.typ.IsDir() && src.typ.IsDir() {
			return patherr("rename", np, simplefs.ErrNotDir)
		}
		if dst.typ.IsDir() && len(dst.children) > 0 {
			return patherr("rename", np, simplefs.ErrNotEmpty)
		}
	}

	srcParent.delChild(srcName)
	dstParent.setChild(dstName, src)
	if replacing {
		m.unlinkLocked(dst)
	}
	for h := range m.handles {
		if simplefs.IsUnder(op, h.path) {
			h.path = np + h.path[len(op):]
		}
	}
	now := time.Now()
	srcParent.modTime, dstParent.modTime = now, now
	srcParent.bump()
	dstParent.bump()
	src.bump()
	m.event(simplefs.EventRename, np, op, src)
	return nil
}

// Readlink returns the target of the symlink at path.
func (m *FS) Readlink(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p := simplefs.CleanPath(path)
	m.mu.RLock()
	defer m.mu.RUnlock()

	n, err := m.walk(p)
	if err != nil {
		return "", patherr("readlink", p, err)
	}
	if !n.typ.IsSymlink() {
		return "", patherr("readlink", p, simplefs.ErrInvalid)
	}
	return n.target, nil
}

// Symlink creates a symbolic link at linkPath pointing to target.
func (m *FS) Symlink(ctx context.Context, target, linkPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p := simplefs.CleanPath(linkPath)
	m.mu.Lock()
	defer m.mu.Unlock()

	parent, name, err := m.walkParent(p)
	if err != nil {
		return patherr("symlink", p, err)
	}
	if _, ok := parent.children[name]; ok {
		return patherr("symlink", p, simplefs.ErrExist)
	}
	n := &node{typ: simplefs.TypeSymlink, mode: 0o777, modTime: time.Now(), target: target, version: 1, nlink: 1, ino: m.newIno()}
	parent.setChild(name, n)
	parent.modTime = n.modTime
	parent.bump()
	m.event(simplefs.EventCreate, p, "", n)
	return nil
}

// Link creates a hard link: newPath becomes another name for the file at
// existingPath (the two share content, mode, and times).
func (m *FS) Link(ctx context.Context, existingPath, newPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ep, np := simplefs.CleanPath(existingPath), simplefs.CleanPath(newPath)
	m.mu.Lock()
	defer m.mu.Unlock()

	n, err := m.walk(ep)
	if err != nil {
		return patherr("link", ep, err)
	}
	if n.typ.IsDir() {
		return patherr("link", ep, simplefs.ErrIsDir)
	}
	parent, name, err := m.walkParent(np)
	if err != nil {
		return patherr("link", np, err)
	}
	if _, ok := parent.children[name]; ok {
		return patherr("link", np, simplefs.ErrExist)
	}
	parent.setChild(name, n)
	m.linkLocked(n)
	parent.modTime = time.Now()
	parent.bump()
	m.event(simplefs.EventCreate, np, "", n)
	return nil
}

// Chmod changes the permission bits of the object at path.
func (m *FS) Chmod(ctx context.Context, path string, mode simplefs.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return m.setMeta("chmod", path, func(n *node) { n.mode = mode })
}

// Chown changes the ownership of the object at path; -1 leaves an id
// unchanged.
func (m *FS) Chown(ctx context.Context, path string, uid, gid int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return m.setMeta("chown", path, func(n *node) {
		if uid >= 0 {
			n.uid = uid
		}
		if gid >= 0 {
			n.gid = gid
		}
	})
}

// Chtimes changes the modification time of the object at path (atime is
// accepted for interface compatibility but not stored). A zero mtime leaves
// the modification time unchanged.
func (m *FS) Chtimes(ctx context.Context, path string, atime, mtime time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return m.setMeta("chtimes", path, func(n *node) {
		if !mtime.IsZero() {
			n.modTime = mtime
		}
	})
}

func (m *FS) setMeta(op, path string, apply func(*node)) error {
	p := simplefs.CleanPath(path)
	m.mu.Lock()
	defer m.mu.Unlock()

	n, err := m.walk(p)
	if err != nil {
		return patherr(op, p, err)
	}
	apply(n)
	n.bump()
	m.event(simplefs.EventModify, p, "", n)
	return nil
}

// GetXattr returns the value of the named extended attribute.
func (m *FS) GetXattr(ctx context.Context, path, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := simplefs.CleanPath(path)
	m.mu.RLock()
	defer m.mu.RUnlock()

	n, err := m.walk(p)
	if err != nil {
		return nil, patherr("getxattr", p, err)
	}
	v, ok := n.xattr[name]
	if !ok {
		return nil, patherr("getxattr", p, simplefs.ErrNotExist)
	}
	return append([]byte(nil), v...), nil
}

// SetXattr sets the named extended attribute.
func (m *FS) SetXattr(ctx context.Context, path, name string, value []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return m.setMeta("setxattr", path, func(n *node) {
		if n.xattr == nil {
			n.xattr = map[string][]byte{}
		}
		n.xattr[name] = append([]byte(nil), value...)
	})
}

// ListXattr returns the names of all extended attributes, sorted.
func (m *FS) ListXattr(ctx context.Context, path string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := simplefs.CleanPath(path)
	m.mu.RLock()
	defer m.mu.RUnlock()

	n, err := m.walk(p)
	if err != nil {
		return nil, patherr("listxattr", p, err)
	}
	return slices.Sorted(maps.Keys(n.xattr)), nil
}

// RemoveXattr removes the named extended attribute.
func (m *FS) RemoveXattr(ctx context.Context, path, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p := simplefs.CleanPath(path)
	m.mu.Lock()
	defer m.mu.Unlock()

	n, err := m.walk(p)
	if err != nil {
		return patherr("removexattr", p, err)
	}
	if _, ok := n.xattr[name]; !ok {
		return patherr("removexattr", p, simplefs.ErrNotExist)
	}
	delete(n.xattr, name)
	n.bump()
	m.event(simplefs.EventModify, p, "", n)
	return nil
}

// StatFS reports the configured capacity (see WithCapacity), with usage
// derived from the bytes currently stored.
func (m *FS) StatFS(ctx context.Context) (simplefs.FSInfo, error) {
	if err := ctx.Err(); err != nil {
		return simplefs.FSInfo{}, err
	}
	m.mu.RLock()
	usedBytes := uint64(m.usedBytes)
	m.mu.RUnlock()

	totalBlocks := uint64(m.capacity) / blockSize
	used := (usedBytes + blockSize - 1) / blockSize
	free := totalBlocks
	if used < free {
		free -= used
	} else {
		free = 0
	}
	return simplefs.FSInfo{
		BlockSize:     blockSize,
		Blocks:        totalBlocks,
		Free:          free,
		Available:     free,
		MaxNameLength: 255,
	}, nil
}

// Watch streams events for path (which must exist): the path itself, its
// direct children, and — when recursive — all descendants. The channel is
// closed when ctx is cancelled; always cancel the context to release the
// watcher. Slow receivers drop events instead of blocking the filesystem.
func (m *FS) Watch(ctx context.Context, path string, recursive bool) (<-chan simplefs.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := simplefs.CleanPath(path)

	m.mu.Lock()
	if _, err := m.walk(p); err != nil {
		m.mu.Unlock()
		return nil, patherr("watch", p, err)
	}
	w := &watcher{path: p, recursive: recursive, ch: make(chan simplefs.Event, 64)}
	m.watchers[w] = struct{}{}
	m.mu.Unlock()

	go func() {
		<-ctx.Done()
		m.mu.Lock()
		delete(m.watchers, w)
		m.mu.Unlock()
		close(w.ch)
	}()
	return w.ch, nil
}

type handle struct {
	m    *FS
	n    *node
	path string
	opts simplefs.OpenOptions

	mu     sync.Mutex
	closed bool
	dirty  bool // modified since the last modify event
}

var _ simplefs.FileHandle = (*handle)(nil)

// flushEvent emits one modify event for all writes since the last flush.
// Coalescing keeps a large copy from flooding watchers with an event (and a
// FileInfo allocation) per WriteAt.
func (h *handle) flushEvent() {
	h.mu.Lock()
	dirty := h.dirty
	h.dirty = false
	h.mu.Unlock()
	if !dirty {
		return
	}
	h.m.mu.RLock()
	h.m.event(simplefs.EventModify, h.path, "", h.n)
	h.m.mu.RUnlock()
}

func (h *handle) markDirty() {
	h.mu.Lock()
	h.dirty = true
	h.mu.Unlock()
}

func (h *handle) check(write bool) error {
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		return patherr("io", h.path, simplefs.ErrClosed)
	}
	if write && !h.opts.Write {
		return patherr("write", h.path, simplefs.ErrPermission)
	}
	if !write && !h.opts.Read {
		return patherr("read", h.path, simplefs.ErrPermission)
	}
	return nil
}

func (h *handle) ReadAt(p []byte, off int64) (int, error) {
	if err := h.check(false); err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, patherr("read", h.path, simplefs.ErrInvalid)
	}
	h.m.mu.RLock()
	defer h.m.mu.RUnlock()

	if off >= int64(len(h.n.data)) {
		return 0, io.EOF
	}
	n := copy(p, h.n.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (h *handle) WriteAt(p []byte, off int64) (int, error) {
	if err := h.check(true); err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, patherr("write", h.path, simplefs.ErrInvalid)
	}
	h.m.mu.Lock()
	defer h.m.mu.Unlock()

	if h.opts.Append {
		off = int64(len(h.n.data))
	}
	end := off + int64(len(p))
	if end < off || !h.m.reserveLocked(h.n, end) {
		return 0, patherr("write", h.path, simplefs.ErrNoSpace)
	}
	if end > int64(len(h.n.data)) {
		h.m.setDataLocked(h.n, resize(h.n.data, end))
	}
	copy(h.n.data[off:], p)
	h.n.modTime = time.Now()
	h.n.bump()
	h.markDirty()
	return len(p), nil
}

// Stat returns the file's current metadata, reporting the path it has now
// (after any renames since it was opened).
func (h *handle) Stat(ctx context.Context) (simplefs.FileInfo, error) {
	h.m.mu.RLock()
	defer h.m.mu.RUnlock()
	return h.n.info(h.path), nil
}

// Sync emits the modify event for writes since the last Sync or Close.
func (h *handle) Sync(ctx context.Context) error {
	h.flushEvent()
	return nil
}

func (h *handle) Truncate(ctx context.Context, size int64) error {
	if err := h.check(true); err != nil {
		return err
	}
	if size < 0 {
		return patherr("truncate", h.path, simplefs.ErrInvalid)
	}
	h.m.mu.Lock()
	defer h.m.mu.Unlock()

	if size == int64(len(h.n.data)) {
		return nil
	}
	if !h.m.reserveLocked(h.n, size) {
		return patherr("truncate", h.path, simplefs.ErrNoSpace)
	}
	h.m.setDataLocked(h.n, resize(h.n.data, size))
	h.n.modTime = time.Now()
	h.n.bump()
	h.markDirty()
	return nil
}

// resize returns data with length n. Growth is amortized (append's doubling
// strategy), so sequential writes cost O(n) overall instead of O(n²); bytes
// between the old and new length are zero, which is what a gap left by a
// write past EOF, or a truncate that grows the file, must read back as.
func resize(data []byte, n int64) []byte {
	old := int64(len(data))
	if n <= old {
		// Clear the discarded tail so a later grow within capacity reads
		// zeros rather than stale bytes.
		clear(data[n:old])
		return data[:n]
	}
	if n <= int64(cap(data)) {
		data = data[:n]
		clear(data[old:])
		return data
	}
	return append(data, make([]byte, n-old)...)
}

func (h *handle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return patherr("close", h.path, simplefs.ErrClosed)
	}
	h.closed = true
	dirty := h.dirty
	h.dirty = false
	h.m.mu.Lock()
	delete(h.m.handles, h)
	if dirty {
		h.m.event(simplefs.EventModify, h.path, "", h.n)
	}
	h.m.mu.Unlock()
	return nil
}
