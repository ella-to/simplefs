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
	"sort"
	"strconv"
	"strings"
	"sync"
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

	data     []byte           // TypeFile
	children map[string]*node // TypeDirectory
	target   string           // TypeSymlink
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
		Name:    simplefs.BaseName(path),
		Type:    n.typ,
		Size:    n.size(),
		Mode:    n.mode,
		ModTime: n.modTime,
		ETag:    strconv.FormatUint(n.version, 10),
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
	if p == w.path {
		return true
	}
	prefix := w.path + "/"
	if w.path == "/" {
		prefix = "/"
	}
	if !strings.HasPrefix(p, prefix) {
		return false
	}
	return w.recursive || !strings.Contains(p[len(prefix):], "/")
}

// MemFS is an in-memory simplefs.FileProvider. The zero value is not usable; use
// New. All methods are safe for concurrent use.
type MemFS struct {
	mu       sync.RWMutex
	root     *node
	watchers map[*watcher]struct{}
}

var (
	_ simplefs.FileProvider     = (*MemFS)(nil)
	_ simplefs.SymlinkProvider  = (*MemFS)(nil)
	_ simplefs.HardLinkProvider = (*MemFS)(nil)
	_ simplefs.ChmodProvider    = (*MemFS)(nil)
	_ simplefs.ChownProvider    = (*MemFS)(nil)
	_ simplefs.ChtimesProvider  = (*MemFS)(nil)
	_ simplefs.XattrProvider    = (*MemFS)(nil)
	_ simplefs.StatFSProvider   = (*MemFS)(nil)
	_ simplefs.WatchProvider    = (*MemFS)(nil)
)

// New returns an empty in-memory filesystem.
func New() *MemFS {
	return &MemFS{
		root: &node{
			typ:      simplefs.TypeDirectory,
			mode:     0o755,
			modTime:  time.Now(),
			children: map[string]*node{},
			version:  1,
		},
		watchers: map[*watcher]struct{}{},
	}
}

func patherr(op, path string, err error) error {
	return &simplefs.PathError{Op: op, Path: path, Err: err}
}

// walk returns the node at path without following symlinks (Lstat semantics).
// Callers must hold m.mu.
func (m *MemFS) walk(path string) (*node, error) {
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
func (m *MemFS) walkParent(path string) (*node, string, error) {
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
	return parent, simplefs.BaseName(p), nil
}

// notify delivers ev to every matching watcher. Callers must hold m.mu (read
// or write); sends never block — a slow watcher drops events.
func (m *MemFS) notify(ev simplefs.Event) {
	for w := range m.watchers {
		if w.matches(ev.Path) || (ev.OldPath != "" && w.matches(ev.OldPath)) {
			select {
			case w.ch <- ev:
			default:
			}
		}
	}
}

func (m *MemFS) event(typ simplefs.EventType, path, oldPath string, n *node) {
	var info *simplefs.FileInfo
	if n != nil {
		fi := n.info(path)
		info = &fi
	}
	m.notify(simplefs.Event{Type: typ, Path: path, OldPath: oldPath, Info: info})
}

// Stat returns metadata for the object at path (without following symlinks).
func (m *MemFS) Stat(ctx context.Context, path string) (simplefs.FileInfo, error) {
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
// created (mode 0o644); with opts.Exclusive set an existing file is an error.
func (m *MemFS) Open(ctx context.Context, path string, opts simplefs.OpenOptions) (simplefs.FileHandle, error) {
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
		n, err = m.createFileLocked(p, 0o644)
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
		n.data = nil
		n.modTime = time.Now()
		n.bump()
		m.event(simplefs.EventModify, p, "", n)
	}
	return &handle{m: m, n: n, path: p, opts: opts}, nil
}

// createFileLocked inserts a new empty file node at p. Callers must hold m.mu
// for writing.
func (m *MemFS) createFileLocked(p string, mode simplefs.FileMode) (*node, error) {
	parent, name, err := m.walkParent(p)
	if err != nil {
		return nil, err
	}
	n := &node{typ: simplefs.TypeFile, mode: mode, modTime: time.Now(), version: 1}
	parent.children[name] = n
	parent.modTime = n.modTime
	parent.bump()
	m.event(simplefs.EventCreate, p, "", n)
	return n, nil
}

// Create creates (or truncates) the file at path and opens it read/write.
func (m *MemFS) Create(ctx context.Context, path string, opts simplefs.CreateOptions) (simplefs.FileHandle, error) {
	mode := opts.Mode
	if mode == 0 {
		mode = 0o644
	}
	p := simplefs.CleanPath(path)

	m.mu.Lock()
	defer m.mu.Unlock()

	n, err := m.walk(p)
	switch {
	case err == nil:
		if n.typ.IsDir() {
			return nil, patherr("create", p, simplefs.ErrIsDir)
		}
		if n.typ.IsSymlink() {
			return nil, patherr("create", p, simplefs.ErrNotSupported)
		}
		if len(n.data) > 0 {
			n.data = nil
			n.modTime = time.Now()
			n.bump()
			m.event(simplefs.EventModify, p, "", n)
		}
	case errors.Is(err, simplefs.ErrNotExist):
		n, err = m.createFileLocked(p, mode)
		if err != nil {
			return nil, patherr("create", p, err)
		}
	default:
		return nil, patherr("create", p, err)
	}
	return &handle{m: m, n: n, path: p, opts: simplefs.OpenOptions{Read: true, Write: true}}, nil
}

// ReadDir lists the directory at path. Pagination: entries are sorted by
// name, Page.Token is the last name of the previous page.
func (m *MemFS) ReadDir(ctx context.Context, path string, page simplefs.Page) (simplefs.DirPage, error) {
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

	names := make([]string, 0, len(n.children))
	for name := range n.children {
		names = append(names, name)
	}
	sort.Strings(names)

	if page.Token != "" {
		idx := sort.SearchStrings(names, page.Token)
		if idx < len(names) && names[idx] == page.Token {
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
func (m *MemFS) Mkdir(ctx context.Context, path string, opts simplefs.MkdirOptions) error {
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
		typ:      simplefs.TypeDirectory,
		mode:     mode,
		modTime:  time.Now(),
		children: map[string]*node{},
		version:  1,
	}
	parent.children[name] = n
	parent.modTime = n.modTime
	parent.bump()
	m.event(simplefs.EventCreate, p, "", n)
	return nil
}

// Remove deletes the object at path. Non-empty directories require
// opts.Recursive; opts.Force turns a missing path into a no-op.
func (m *MemFS) Remove(ctx context.Context, path string, opts simplefs.RemoveOptions) error {
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
	delete(parent.children, name)
	parent.modTime = time.Now()
	parent.bump()
	m.event(simplefs.EventDelete, p, "", n)
	return nil
}

// Rename moves oldPath to newPath. An existing destination requires
// opts.Overwrite and must be type-compatible (and, for directories, empty).
func (m *MemFS) Rename(ctx context.Context, oldPath, newPath string, opts simplefs.RenameOptions) error {
	op, np := simplefs.CleanPath(oldPath), simplefs.CleanPath(newPath)
	if op == "/" || np == "/" {
		return patherr("rename", op, simplefs.ErrInvalid)
	}
	if op == np {
		return nil
	}
	if strings.HasPrefix(np, op+"/") {
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
	if dst, ok := dstParent.children[dstName]; ok {
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

	delete(srcParent.children, srcName)
	dstParent.children[dstName] = src
	now := time.Now()
	srcParent.modTime, dstParent.modTime = now, now
	srcParent.bump()
	dstParent.bump()
	src.bump()
	m.event(simplefs.EventRename, np, op, src)
	return nil
}

// Readlink returns the target of the symlink at path.
func (m *MemFS) Readlink(ctx context.Context, path string) (string, error) {
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
func (m *MemFS) Symlink(ctx context.Context, target, linkPath string) error {
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
	n := &node{typ: simplefs.TypeSymlink, mode: 0o777, modTime: time.Now(), target: target, version: 1}
	parent.children[name] = n
	parent.modTime = n.modTime
	parent.bump()
	m.event(simplefs.EventCreate, p, "", n)
	return nil
}

// Link creates a hard link: newPath becomes another name for the file at
// existingPath (the two share content, mode, and times).
func (m *MemFS) Link(ctx context.Context, existingPath, newPath string) error {
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
	parent.children[name] = n
	parent.modTime = time.Now()
	parent.bump()
	m.event(simplefs.EventCreate, np, "", n)
	return nil
}

// Chmod changes the permission bits of the object at path.
func (m *MemFS) Chmod(ctx context.Context, path string, mode simplefs.FileMode) error {
	return m.setMeta("chmod", path, func(n *node) { n.mode = mode })
}

// Chown changes the ownership of the object at path.
func (m *MemFS) Chown(ctx context.Context, path string, uid, gid int) error {
	return m.setMeta("chown", path, func(n *node) { n.uid, n.gid = uid, gid })
}

// Chtimes changes the modification time of the object at path (atime is
// accepted for interface compatibility but not stored).
func (m *MemFS) Chtimes(ctx context.Context, path string, atime, mtime time.Time) error {
	return m.setMeta("chtimes", path, func(n *node) { n.modTime = mtime })
}

func (m *MemFS) setMeta(op, path string, apply func(*node)) error {
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
func (m *MemFS) GetXattr(ctx context.Context, path, name string) ([]byte, error) {
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
func (m *MemFS) SetXattr(ctx context.Context, path, name string, value []byte) error {
	return m.setMeta("setxattr", path, func(n *node) {
		if n.xattr == nil {
			n.xattr = map[string][]byte{}
		}
		n.xattr[name] = append([]byte(nil), value...)
	})
}

// ListXattr returns the names of all extended attributes, sorted.
func (m *MemFS) ListXattr(ctx context.Context, path string) ([]string, error) {
	p := simplefs.CleanPath(path)
	m.mu.RLock()
	defer m.mu.RUnlock()

	n, err := m.walk(p)
	if err != nil {
		return nil, patherr("listxattr", p, err)
	}
	names := make([]string, 0, len(n.xattr))
	for name := range n.xattr {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// RemoveXattr removes the named extended attribute.
func (m *MemFS) RemoveXattr(ctx context.Context, path, name string) error {
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

// StatFS reports capacity for a pretend 1 GiB volume, with usage derived from
// the bytes currently stored.
func (m *MemFS) StatFS(ctx context.Context) (simplefs.FSInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var usedBytes uint64
	var visit func(*node)
	visit = func(n *node) {
		usedBytes += uint64(len(n.data))
		for _, c := range n.children {
			visit(c)
		}
	}
	visit(m.root)

	const totalBlocks = (1 << 30) / blockSize
	used := (usedBytes + blockSize - 1) / blockSize
	free := uint64(totalBlocks)
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
func (m *MemFS) Watch(ctx context.Context, path string, recursive bool) (<-chan simplefs.Event, error) {
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
	m    *MemFS
	n    *node
	path string
	opts simplefs.OpenOptions

	mu     sync.Mutex
	closed bool
}

var _ simplefs.FileHandle = (*handle)(nil)

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
	if end := off + int64(len(p)); end > int64(len(h.n.data)) {
		grown := make([]byte, end)
		copy(grown, h.n.data)
		h.n.data = grown
	}
	copy(h.n.data[off:], p)
	h.n.modTime = time.Now()
	h.n.bump()
	h.m.event(simplefs.EventModify, h.path, "", h.n)
	return len(p), nil
}

func (h *handle) Stat(ctx context.Context) (simplefs.FileInfo, error) {
	h.m.mu.RLock()
	defer h.m.mu.RUnlock()
	return h.n.info(h.path), nil
}

func (h *handle) Sync(ctx context.Context) error { return nil }

func (h *handle) Truncate(ctx context.Context, size int64) error {
	if err := h.check(true); err != nil {
		return err
	}
	if size < 0 {
		return patherr("truncate", h.path, simplefs.ErrInvalid)
	}
	h.m.mu.Lock()
	defer h.m.mu.Unlock()

	switch {
	case size < int64(len(h.n.data)):
		h.n.data = h.n.data[:size]
	case size > int64(len(h.n.data)):
		grown := make([]byte, size)
		copy(grown, h.n.data)
		h.n.data = grown
	default:
		return nil
	}
	h.n.modTime = time.Now()
	h.n.bump()
	h.m.event(simplefs.EventModify, h.path, "", h.n)
	return nil
}

func (h *handle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return patherr("close", h.path, simplefs.ErrClosed)
	}
	h.closed = true
	return nil
}
