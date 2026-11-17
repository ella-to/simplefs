package simplefs

import (
	"context"
	"errors"
	"time"
)

// Unwrapper is implemented by provider middleware (logging, metrics,
// caching, ...) that forwards calls unchanged to an inner provider. As uses
// it to find capabilities the wrapper does not implement itself.
//
// Only implement Unwrap when calls reaching the inner provider directly
// would behave the same as calls through the wrapper. Wrappers that change
// paths or semantics (Sub, ReadOnly) must implement every capability
// themselves and must not implement Unwrap, or As would bypass them.
type Unwrapper interface {
	Unwrap() FileProvider
}

// As finds the first provider in p's Unwrap chain that implements T —
// usually a capability interface — and returns it. Backends use As instead
// of a plain type assertion so capabilities stay visible through
// middleware:
//
//	if sp, ok := simplefs.As[simplefs.SymlinkProvider](p); ok { ... }
func As[T any](p FileProvider) (T, bool) {
	for p != nil {
		if t, ok := p.(T); ok {
			return t, true
		}
		u, ok := p.(Unwrapper)
		if !ok {
			break
		}
		p = u.Unwrap()
	}
	var zero T
	return zero, false
}

// ReadOnly returns a view of p that rejects every mutation with ErrReadOnly
// and reports FSInfo.ReadOnly. Reads, listings, symlink and xattr reads, and
// watches pass through. Capabilities p lacks report ErrNotSupported.
func ReadOnly(p FileProvider) FileProvider { return readOnlyFS{p} }

type readOnlyFS struct{ p FileProvider }

var (
	_ SymlinkProvider  = readOnlyFS{}
	_ HardLinkProvider = readOnlyFS{}
	_ ChmodProvider    = readOnlyFS{}
	_ ChownProvider    = readOnlyFS{}
	_ ChtimesProvider  = readOnlyFS{}
	_ XattrProvider    = readOnlyFS{}
	_ StatFSProvider   = readOnlyFS{}
	_ WatchProvider    = readOnlyFS{}
)

func roErr(op, path string) error { return &PathError{Op: op, Path: path, Err: ErrReadOnly} }

func unsupported(op, path string) error { return &PathError{Op: op, Path: path, Err: ErrNotSupported} }

func (r readOnlyFS) Stat(ctx context.Context, path string) (FileInfo, error) {
	return r.p.Stat(ctx, path)
}

func (r readOnlyFS) Open(ctx context.Context, path string, opts OpenOptions) (FileHandle, error) {
	if opts.Write || opts.Create || opts.Truncate || opts.Append {
		return nil, roErr("open", path)
	}
	h, err := r.p.Open(ctx, path, opts)
	if err != nil {
		return nil, err
	}
	return readOnlyHandle{h, path}, nil
}

func (r readOnlyFS) ReadDir(ctx context.Context, path string, page Page) (DirPage, error) {
	return r.p.ReadDir(ctx, path, page)
}

func (r readOnlyFS) Mkdir(ctx context.Context, path string, opts MkdirOptions) error {
	return roErr("mkdir", path)
}

func (r readOnlyFS) Remove(ctx context.Context, path string, opts RemoveOptions) error {
	return roErr("remove", path)
}

func (r readOnlyFS) Rename(ctx context.Context, oldPath, newPath string, opts RenameOptions) error {
	return roErr("rename", oldPath)
}

func (r readOnlyFS) Readlink(ctx context.Context, path string) (string, error) {
	if sp, ok := As[SymlinkProvider](r.p); ok {
		return sp.Readlink(ctx, path)
	}
	return "", unsupported("readlink", path)
}

func (r readOnlyFS) Symlink(ctx context.Context, target, linkPath string) error {
	return roErr("symlink", linkPath)
}

func (r readOnlyFS) Link(ctx context.Context, existingPath, newPath string) error {
	return roErr("link", newPath)
}

func (r readOnlyFS) Chmod(ctx context.Context, path string, mode FileMode) error {
	return roErr("chmod", path)
}

func (r readOnlyFS) Chown(ctx context.Context, path string, uid, gid int) error {
	return roErr("chown", path)
}

func (r readOnlyFS) Chtimes(ctx context.Context, path string, atime, mtime time.Time) error {
	return roErr("chtimes", path)
}

func (r readOnlyFS) GetXattr(ctx context.Context, path, name string) ([]byte, error) {
	if xp, ok := As[XattrProvider](r.p); ok {
		return xp.GetXattr(ctx, path, name)
	}
	return nil, unsupported("getxattr", path)
}

func (r readOnlyFS) SetXattr(ctx context.Context, path, name string, value []byte) error {
	return roErr("setxattr", path)
}

func (r readOnlyFS) ListXattr(ctx context.Context, path string) ([]string, error) {
	if xp, ok := As[XattrProvider](r.p); ok {
		return xp.ListXattr(ctx, path)
	}
	return nil, unsupported("listxattr", path)
}

func (r readOnlyFS) RemoveXattr(ctx context.Context, path, name string) error {
	return roErr("removexattr", path)
}

func (r readOnlyFS) StatFS(ctx context.Context) (FSInfo, error) {
	info := FSInfo{MaxNameLength: MaxNameLength}
	if sp, ok := As[StatFSProvider](r.p); ok {
		var err error
		if info, err = sp.StatFS(ctx); err != nil {
			return FSInfo{}, err
		}
	}
	info.ReadOnly = true
	return info, nil
}

func (r readOnlyFS) Watch(ctx context.Context, path string, recursive bool) (<-chan Event, error) {
	if wp, ok := As[WatchProvider](r.p); ok {
		return wp.Watch(ctx, path, recursive)
	}
	return nil, unsupported("watch", path)
}

type readOnlyHandle struct {
	FileHandle
	path string
}

func (h readOnlyHandle) WriteAt(p []byte, off int64) (int, error) {
	return 0, roErr("write", h.path)
}

func (h readOnlyHandle) Truncate(ctx context.Context, size int64) error {
	return roErr("truncate", h.path)
}

// Sub returns a view of the directory dir of p as a provider of its own:
// "/" is dir, and no path can reach outside it ("..", being resolved by
// CleanPath, stops at the new root). Paths in returned FileInfos, events,
// and *PathErrors are relative to the new root. Capabilities p lacks report
// ErrNotSupported. dir must exist when the view is used.
func Sub(p FileProvider, dir string) FileProvider {
	dir = CleanPath(dir)
	if dir == "/" {
		return p
	}
	if s, ok := p.(subFS); ok {
		return subFS{p: s.p, dir: JoinPath(s.dir, dir)}
	}
	return subFS{p: p, dir: dir}
}

type subFS struct {
	p   FileProvider
	dir string
}

var (
	_ SymlinkProvider  = subFS{}
	_ HardLinkProvider = subFS{}
	_ ChmodProvider    = subFS{}
	_ ChownProvider    = subFS{}
	_ ChtimesProvider  = subFS{}
	_ XattrProvider    = subFS{}
	_ StatFSProvider   = subFS{}
	_ WatchProvider    = subFS{}
)

// in maps a path of the view to a path of the inner provider.
func (s subFS) in(path string) string { return JoinPath(s.dir, CleanPath(path)) }

// out maps an inner path back into the view.
func (s subFS) out(path string) string {
	if path == s.dir {
		return "/"
	}
	return path[len(s.dir):]
}

func (s subFS) info(fi FileInfo) FileInfo {
	if IsUnder(s.dir, fi.Path) {
		fi.Path = s.out(CleanPath(fi.Path))
	}
	return fi
}

// err rewrites the path of a *PathError from the inner provider.
func (s subFS) err(err error) error {
	var pe *PathError
	if errors.As(err, &pe) && IsUnder(s.dir, pe.Path) {
		return &PathError{Op: pe.Op, Path: s.out(CleanPath(pe.Path)), Err: pe.Err}
	}
	return err
}

func (s subFS) Stat(ctx context.Context, path string) (FileInfo, error) {
	fi, err := s.p.Stat(ctx, s.in(path))
	if err != nil {
		return FileInfo{}, s.err(err)
	}
	return s.info(fi), nil
}

func (s subFS) Open(ctx context.Context, path string, opts OpenOptions) (FileHandle, error) {
	h, err := s.p.Open(ctx, s.in(path), opts)
	if err != nil {
		return nil, s.err(err)
	}
	return subHandle{h, s}, nil
}

func (s subFS) ReadDir(ctx context.Context, path string, page Page) (DirPage, error) {
	dp, err := s.p.ReadDir(ctx, s.in(path), page)
	if err != nil {
		return DirPage{}, s.err(err)
	}
	for i, e := range dp.Entries {
		if e.Info != nil {
			fi := s.info(*e.Info)
			dp.Entries[i].Info = &fi
		}
	}
	return dp, nil
}

func (s subFS) Mkdir(ctx context.Context, path string, opts MkdirOptions) error {
	return s.err(s.p.Mkdir(ctx, s.in(path), opts))
}

func (s subFS) Remove(ctx context.Context, path string, opts RemoveOptions) error {
	if CleanPath(path) == "/" {
		return &PathError{Op: "remove", Path: "/", Err: ErrInvalid}
	}
	return s.err(s.p.Remove(ctx, s.in(path), opts))
}

func (s subFS) Rename(ctx context.Context, oldPath, newPath string, opts RenameOptions) error {
	if CleanPath(oldPath) == "/" || CleanPath(newPath) == "/" {
		return &PathError{Op: "rename", Path: "/", Err: ErrInvalid}
	}
	return s.err(s.p.Rename(ctx, s.in(oldPath), s.in(newPath), opts))
}

func (s subFS) Readlink(ctx context.Context, path string) (string, error) {
	sp, ok := As[SymlinkProvider](s.p)
	if !ok {
		return "", unsupported("readlink", path)
	}
	target, err := sp.Readlink(ctx, s.in(path))
	return target, s.err(err)
}

func (s subFS) Symlink(ctx context.Context, target, linkPath string) error {
	sp, ok := As[SymlinkProvider](s.p)
	if !ok {
		return unsupported("symlink", linkPath)
	}
	return s.err(sp.Symlink(ctx, target, s.in(linkPath)))
}

func (s subFS) Link(ctx context.Context, existingPath, newPath string) error {
	hp, ok := As[HardLinkProvider](s.p)
	if !ok {
		return unsupported("link", newPath)
	}
	return s.err(hp.Link(ctx, s.in(existingPath), s.in(newPath)))
}

func (s subFS) Chmod(ctx context.Context, path string, mode FileMode) error {
	cp, ok := As[ChmodProvider](s.p)
	if !ok {
		return unsupported("chmod", path)
	}
	return s.err(cp.Chmod(ctx, s.in(path), mode))
}

func (s subFS) Chown(ctx context.Context, path string, uid, gid int) error {
	cp, ok := As[ChownProvider](s.p)
	if !ok {
		return unsupported("chown", path)
	}
	return s.err(cp.Chown(ctx, s.in(path), uid, gid))
}

func (s subFS) Chtimes(ctx context.Context, path string, atime, mtime time.Time) error {
	cp, ok := As[ChtimesProvider](s.p)
	if !ok {
		return unsupported("chtimes", path)
	}
	return s.err(cp.Chtimes(ctx, s.in(path), atime, mtime))
}

func (s subFS) GetXattr(ctx context.Context, path, name string) ([]byte, error) {
	xp, ok := As[XattrProvider](s.p)
	if !ok {
		return nil, unsupported("getxattr", path)
	}
	v, err := xp.GetXattr(ctx, s.in(path), name)
	return v, s.err(err)
}

func (s subFS) SetXattr(ctx context.Context, path, name string, value []byte) error {
	xp, ok := As[XattrProvider](s.p)
	if !ok {
		return unsupported("setxattr", path)
	}
	return s.err(xp.SetXattr(ctx, s.in(path), name, value))
}

func (s subFS) ListXattr(ctx context.Context, path string) ([]string, error) {
	xp, ok := As[XattrProvider](s.p)
	if !ok {
		return nil, unsupported("listxattr", path)
	}
	v, err := xp.ListXattr(ctx, s.in(path))
	return v, s.err(err)
}

func (s subFS) RemoveXattr(ctx context.Context, path, name string) error {
	xp, ok := As[XattrProvider](s.p)
	if !ok {
		return unsupported("removexattr", path)
	}
	return s.err(xp.RemoveXattr(ctx, s.in(path), name))
}

func (s subFS) StatFS(ctx context.Context) (FSInfo, error) {
	sp, ok := As[StatFSProvider](s.p)
	if !ok {
		return FSInfo{}, unsupported("statfs", "/")
	}
	return sp.StatFS(ctx)
}

// Watch translates the inner provider's events into the view: paths are
// rewritten, events outside the view dropped, and renames across its edge
// become creates (moved in) or deletes (moved out).
func (s subFS) Watch(ctx context.Context, path string, recursive bool) (<-chan Event, error) {
	wp, ok := As[WatchProvider](s.p)
	if !ok {
		return nil, unsupported("watch", path)
	}
	in, err := wp.Watch(ctx, s.in(path), recursive)
	if err != nil {
		return nil, s.err(err)
	}
	out := make(chan Event, cap(in))
	go func() {
		defer close(out)
		for ev := range in {
			newIn := IsUnder(s.dir, ev.Path)
			oldIn := ev.OldPath != "" && IsUnder(s.dir, ev.OldPath)
			switch {
			case newIn && ev.OldPath != "" && !oldIn:
				ev.Type, ev.OldPath = EventCreate, ""
			case !newIn && oldIn:
				ev.Type, ev.Path, ev.OldPath, ev.Info = EventDelete, ev.OldPath, "", nil
			case !newIn:
				continue
			}
			ev.Path = s.out(CleanPath(ev.Path))
			if ev.OldPath != "" {
				ev.OldPath = s.out(CleanPath(ev.OldPath))
			}
			if ev.Info != nil {
				fi := s.info(*ev.Info)
				ev.Info = &fi
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				// Keep draining so the inner provider can close its channel.
			}
		}
	}()
	return out, nil
}

type subHandle struct {
	FileHandle
	s subFS
}

func (h subHandle) Stat(ctx context.Context) (FileInfo, error) {
	fi, err := h.FileHandle.Stat(ctx)
	if err != nil {
		return FileInfo{}, h.s.err(err)
	}
	return h.s.info(fi), nil
}
