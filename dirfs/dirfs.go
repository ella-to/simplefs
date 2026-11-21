// Package dirfs is a simplefs.FileProvider backed by a directory of the
// local filesystem.
//
// All access goes through an os.Root, so no path — not "..", not an absolute
// symlink target — can reach outside the directory. Symlinks inside it work
// normally; a link pointing outside is refused when followed.
//
// Implemented capabilities: SymlinkProvider, HardLinkProvider,
// ChmodProvider, ChownProvider, ChtimesProvider, and StatFSProvider (where
// the OS supports them; otherwise ErrNotSupported). Watching is not
// implemented: the standard library has no portable change notification.
//
// On Windows, permission bits are synthesized from the read-only attribute
// (Chmod can only toggle it), and a file cannot be renamed, nor a directory
// above it, while a handle to it is open.
//
// FileInfo.ETag is left empty: modification times on many filesystems are
// too coarse to version every write, and an ETag that does not change on a
// write is worse than none.
package dirfs

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"ella.to/simplefs"
)

// FS serves a local directory. All methods are safe for concurrent use.
type FS struct {
	root *os.Root
	dir  string

	mu      sync.Mutex
	handles map[*handle]struct{} // open handles, so Rename keeps their paths current
	cursors *cursorCache
}

var (
	_ simplefs.FileProvider     = (*FS)(nil)
	_ simplefs.SymlinkProvider  = (*FS)(nil)
	_ simplefs.HardLinkProvider = (*FS)(nil)
	_ simplefs.ChmodProvider    = (*FS)(nil)
	_ simplefs.ChownProvider    = (*FS)(nil)
	_ simplefs.ChtimesProvider  = (*FS)(nil)
	_ simplefs.StatFSProvider   = (*FS)(nil)
)

// New opens dir, which must exist and be a directory, as a provider. Call
// Close to release it.
func New(dir string) (*FS, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &FS{
		root:    root,
		dir:     dir,
		handles: map[*handle]struct{}{},
		cursors: newCursorCache(),
	}, nil
}

// Close releases the directory. Handles opened earlier stay usable.
func (f *FS) Close() error { return f.root.Close() }

// Dir returns the directory being served.
func (f *FS) Dir() string { return f.dir }

// name converts a logical path to an os.Root name.
func name(path string) string {
	p := simplefs.CleanPath(path)
	if p == "/" {
		return "."
	}
	return p[1:]
}

func infoOf(path string, fi fs.FileInfo) simplefs.FileInfo {
	t := simplefs.TypeFile
	switch {
	case fi.IsDir():
		t = simplefs.TypeDir
	case fi.Mode()&fs.ModeSymlink != 0:
		t = simplefs.TypeSymlink
	}
	ino, links := sysIDs(fi)
	return simplefs.FileInfo{
		Path:    simplefs.CleanPath(path),
		Type:    t,
		Size:    fi.Size(),
		Mode:    fi.Mode().Perm(),
		ModTime: fi.ModTime(),
		FileID:  ino,
		Links:   links,
	}
}

// Stat returns metadata for path without following a final symlink.
func (f *FS) Stat(ctx context.Context, path string) (simplefs.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return simplefs.FileInfo{}, err
	}
	fi, err := f.root.Lstat(name(path))
	if err != nil {
		return simplefs.FileInfo{}, pathErr("stat", path, err)
	}
	return infoOf(path, fi), nil
}

// Open opens or creates the file at path.
func (f *FS) Open(ctx context.Context, path string, opts simplefs.OpenOptions) (simplefs.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := simplefs.CleanPath(path)
	if !opts.Read && !opts.Write {
		opts.Read = true
	}
	if (opts.Truncate || opts.Append) && !opts.Write {
		return nil, pathErr("open", p, simplefs.ErrInvalid)
	}
	if opts.Create {
		if err := checkName(simplefs.BaseName(p)); err != nil {
			return nil, pathErr("open", p, err)
		}
	}
	flag := os.O_RDONLY
	switch {
	case opts.Read && opts.Write:
		flag = os.O_RDWR
	case opts.Write:
		flag = os.O_WRONLY
	}
	if opts.Create {
		flag |= os.O_CREATE
		if opts.Exclusive {
			flag |= os.O_EXCL
		}
	}
	if opts.Truncate {
		flag |= os.O_TRUNC
	}
	if opts.Append {
		// os.File refuses WriteAt on O_APPEND files; append mode is
		// implemented with Write, which the kernel appends atomically.
		flag |= os.O_APPEND
	}
	mode := opts.Mode
	if mode == 0 {
		mode = 0o644
	}
	existed := true
	if opts.Create {
		_, err := f.root.Lstat(name(p))
		existed = err == nil
	}

	file, err := f.root.OpenFile(name(p), flag, mode.Perm())
	if err != nil {
		return nil, pathErr("open", p, err)
	}
	fi, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, pathErr("open", p, err)
	}
	if fi.IsDir() {
		file.Close()
		return nil, pathErr("open", p, simplefs.ErrIsDir)
	}
	if !existed && fi.Mode().Perm() != mode.Perm() {
		// The umask trimmed the requested mode of the file we just
		// created; apply it exactly.
		_ = file.Chmod(mode.Perm())
	}
	h := &handle{fs: f, file: file, path: p, append: opts.Append, write: opts.Write}
	f.mu.Lock()
	f.handles[h] = struct{}{}
	f.mu.Unlock()
	if opts.Create || opts.Truncate {
		f.cursors.invalidate(simplefs.ParentPath(p))
	}
	return h, nil
}

// ReadDir lists the directory at path, in name order. Tokens are the last
// name of the previous page.
func (f *FS) ReadDir(ctx context.Context, path string, page simplefs.Page) (simplefs.DirPage, error) {
	if err := ctx.Err(); err != nil {
		return simplefs.DirPage{}, err
	}
	p := simplefs.CleanPath(path)

	names, ok := f.cursors.take(p, page.Token)
	if !ok {
		var err error
		if names, err = f.listNames(p); err != nil {
			return simplefs.DirPage{}, err
		}
		if page.Token != "" {
			i, found := slices.BinarySearch(names, page.Token)
			if found {
				i++
			}
			names = names[i:]
		}
	}

	limit := len(names)
	if page.Limit > 0 && page.Limit < limit {
		limit = page.Limit
	}
	out := simplefs.DirPage{Entries: make([]simplefs.DirEntry, 0, limit)}
	for _, n := range names[:limit] {
		childPath := simplefs.JoinPath(p, n)
		fi, err := f.root.Lstat(name(childPath))
		if errors.Is(err, fs.ErrNotExist) {
			continue // removed since the listing was taken
		}
		if err != nil {
			return simplefs.DirPage{}, pathErr("readdir", childPath, err)
		}
		info := infoOf(childPath, fi)
		out.Entries = append(out.Entries, simplefs.DirEntry{Name: n, Type: info.Type, Info: &info})
	}
	if limit < len(names) {
		out.NextToken = names[limit-1]
		f.cursors.put(p, out.NextToken, names[limit:])
	}
	return out, nil
}

// listNames returns the sorted names in directory p.
func (f *FS) listNames(p string) ([]string, error) {
	dir, err := f.root.Open(name(p))
	if err != nil {
		return nil, pathErr("readdir", p, err)
	}
	defer dir.Close()
	fi, err := dir.Stat()
	if err != nil {
		return nil, pathErr("readdir", p, err)
	}
	if !fi.IsDir() {
		return nil, pathErr("readdir", p, simplefs.ErrNotDir)
	}
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return nil, pathErr("readdir", p, err)
	}
	slices.Sort(names)
	return names, nil
}

// Mkdir creates a directory.
func (f *FS) Mkdir(ctx context.Context, path string, opts simplefs.MkdirOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p := simplefs.CleanPath(path)
	if p == "/" {
		return pathErr("mkdir", p, simplefs.ErrExist)
	}
	if err := checkName(simplefs.BaseName(p)); err != nil {
		return pathErr("mkdir", p, err)
	}
	mode := opts.Mode
	if mode == 0 {
		mode = 0o755
	}
	if err := f.root.Mkdir(name(p), mode.Perm()); err != nil {
		return pathErr("mkdir", p, err)
	}
	if fi, err := f.root.Lstat(name(p)); err == nil && fi.Mode().Perm() != mode.Perm() {
		_ = f.root.Chmod(name(p), mode.Perm()) // restore bits the umask removed
	}
	f.cursors.invalidate(simplefs.ParentPath(p))
	return nil
}

// Remove removes a file, symlink, or directory.
func (f *FS) Remove(ctx context.Context, path string, opts simplefs.RemoveOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p := simplefs.CleanPath(path)
	if p == "/" {
		return pathErr("remove", p, simplefs.ErrInvalid)
	}
	fi, err := f.root.Lstat(name(p))
	if err != nil {
		if opts.Force && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return pathErr("remove", p, err)
	}
	if fi.IsDir() && opts.Recursive {
		err = f.root.RemoveAll(name(p))
	} else {
		err = f.root.Remove(name(p))
	}
	if err != nil {
		if fi.IsDir() && !opts.Recursive && isNotEmpty(err) {
			return pathErr("remove", p, simplefs.ErrNotEmpty)
		}
		return pathErr("remove", p, err)
	}
	f.cursors.invalidate(simplefs.ParentPath(p))
	return nil
}

// Rename moves oldPath to newPath.
func (f *FS) Rename(ctx context.Context, oldPath, newPath string, opts simplefs.RenameOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	op, np := simplefs.CleanPath(oldPath), simplefs.CleanPath(newPath)
	if op == "/" || np == "/" {
		return pathErr("rename", op, simplefs.ErrInvalid)
	}
	if op == np {
		return nil
	}
	if simplefs.IsUnder(op, np) {
		return pathErr("rename", np, simplefs.ErrInvalid)
	}
	if err := checkName(simplefs.BaseName(np)); err != nil {
		return pathErr("rename", np, err)
	}
	src, err := f.root.Lstat(name(op))
	if err != nil {
		return pathErr("rename", op, err)
	}
	if dst, err := f.root.Lstat(name(np)); err == nil {
		switch {
		case !opts.Overwrite:
			return pathErr("rename", np, simplefs.ErrExist)
		case dst.IsDir() && !src.IsDir():
			return pathErr("rename", np, simplefs.ErrIsDir)
		case !dst.IsDir() && src.IsDir():
			return pathErr("rename", np, simplefs.ErrNotDir)
		case dst.IsDir():
			if names, err := f.listNames(np); err != nil {
				return err
			} else if len(names) > 0 {
				return pathErr("rename", np, simplefs.ErrNotEmpty)
			}
			// os.Rename never replaces a directory (on any OS), so the
			// empty destination is removed first. This is not atomic, but
			// no reader can observe a missing non-empty directory.
			if err := f.root.Remove(name(np)); err != nil {
				return pathErr("rename", np, err)
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return pathErr("rename", np, err)
	}
	if err := f.root.Rename(name(op), name(np)); err != nil {
		return pathErr("rename", op, err)
	}

	f.mu.Lock()
	for h := range f.handles {
		h.mu.Lock()
		if simplefs.IsUnder(op, h.path) {
			h.path = np + h.path[len(op):]
		}
		h.mu.Unlock()
	}
	f.mu.Unlock()
	f.cursors.invalidate(simplefs.ParentPath(op))
	f.cursors.invalidate(simplefs.ParentPath(np))
	return nil
}

// Readlink returns a symlink's target.
func (f *FS) Readlink(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	fi, err := f.root.Lstat(name(path))
	if err != nil {
		return "", pathErr("readlink", path, err)
	}
	if fi.Mode()&fs.ModeSymlink == 0 {
		return "", pathErr("readlink", path, simplefs.ErrInvalid)
	}
	target, err := f.root.Readlink(name(path))
	if err != nil {
		return "", pathErr("readlink", path, err)
	}
	// Windows stores targets with backslashes; simplefs paths use slashes.
	return filepath.ToSlash(target), nil
}

// Symlink creates linkPath pointing at target.
func (f *FS) Symlink(ctx context.Context, target, linkPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkName(simplefs.BaseName(linkPath)); err != nil {
		return pathErr("symlink", linkPath, err)
	}
	if err := f.root.Symlink(target, name(linkPath)); err != nil {
		return pathErr("symlink", linkPath, err)
	}
	f.cursors.invalidate(simplefs.ParentPath(simplefs.CleanPath(linkPath)))
	return nil
}

// Link creates newPath as a hard link to existingPath.
func (f *FS) Link(ctx context.Context, existingPath, newPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fi, err := f.root.Lstat(name(existingPath))
	if err != nil {
		return pathErr("link", existingPath, err)
	}
	if fi.IsDir() {
		return pathErr("link", existingPath, simplefs.ErrIsDir)
	}
	if err := checkName(simplefs.BaseName(newPath)); err != nil {
		return pathErr("link", newPath, err)
	}
	if err := f.root.Link(name(existingPath), name(newPath)); err != nil {
		return pathErr("link", newPath, err)
	}
	f.cursors.invalidate(simplefs.ParentPath(simplefs.CleanPath(newPath)))
	return nil
}

// Chmod changes permission bits.
func (f *FS) Chmod(ctx context.Context, path string, mode simplefs.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return pathErr("chmod", path, f.root.Chmod(name(path), mode.Perm()))
}

// Chown changes ownership.
func (f *FS) Chown(ctx context.Context, path string, uid, gid int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return pathErr("chown", path, f.root.Lchown(name(path), uid, gid))
}

// Chtimes changes access and modification times; a zero time leaves that
// timestamp unchanged.
func (f *FS) Chtimes(ctx context.Context, path string, atime, mtime time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return pathErr("chtimes", path, f.root.Chtimes(name(path), atime, mtime))
}

// StatFS reports the capacity of the filesystem holding the directory.
func (f *FS) StatFS(ctx context.Context) (simplefs.FSInfo, error) {
	if err := ctx.Err(); err != nil {
		return simplefs.FSInfo{}, err
	}
	info, err := statFS(f.dir)
	if err != nil {
		return simplefs.FSInfo{}, pathErr("statfs", "/", err)
	}
	return info, nil
}

// handle is an open file.
type handle struct {
	fs     *FS
	file   *os.File
	append bool
	write  bool

	mu   sync.Mutex
	path string
}

func (h *handle) currentPath() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.path
}

func (h *handle) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, pathErr("read", h.currentPath(), simplefs.ErrInvalid)
	}
	n, err := h.file.ReadAt(p, off)
	if err != nil && err != io.EOF {
		return n, pathErr("read", h.currentPath(), err)
	}
	return n, err
}

func (h *handle) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, pathErr("write", h.currentPath(), simplefs.ErrInvalid)
	}
	if !h.write {
		if _, err := h.file.Stat(); errors.Is(err, os.ErrClosed) {
			return 0, pathErr("write", h.currentPath(), simplefs.ErrClosed)
		}
		return 0, pathErr("write", h.currentPath(), simplefs.ErrPermission)
	}
	var n int
	var err error
	if h.append {
		n, err = h.file.Write(p)
	} else {
		n, err = h.file.WriteAt(p, off)
	}
	if err != nil {
		return n, pathErr("write", h.currentPath(), err)
	}
	return n, nil
}

func (h *handle) Stat(ctx context.Context) (simplefs.FileInfo, error) {
	fi, err := h.file.Stat()
	if err != nil {
		return simplefs.FileInfo{}, pathErr("stat", h.currentPath(), err)
	}
	return infoOf(h.currentPath(), fi), nil
}

func (h *handle) Sync(ctx context.Context) error {
	if err := h.file.Sync(); err != nil {
		return pathErr("sync", h.currentPath(), err)
	}
	return nil
}

func (h *handle) Truncate(ctx context.Context, size int64) error {
	if size < 0 {
		return pathErr("truncate", h.currentPath(), simplefs.ErrInvalid)
	}
	if !h.write {
		return pathErr("truncate", h.currentPath(), simplefs.ErrPermission)
	}
	if err := h.file.Truncate(size); err != nil {
		return pathErr("truncate", h.currentPath(), err)
	}
	return nil
}

func (h *handle) Close() error {
	h.fs.mu.Lock()
	delete(h.fs.handles, h)
	h.fs.mu.Unlock()
	if err := h.file.Close(); err != nil {
		return pathErr("close", h.currentPath(), err)
	}
	return nil
}
