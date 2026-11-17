package simplefs

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"time"
)

// maxSymlinkHops bounds symlink resolution, like the kernel's ELOOP limit.
const maxSymlinkHops = 40

// FS returns p as an io/fs.FS, so the standard library and anything written
// against it (fs.WalkDir, fs.ReadFile, http.FileServerFS,
// template.ParseFS, ...) work on any provider. Every call uses ctx.
//
// The returned value implements fs.ReadDirFS, fs.ReadFileFS, fs.StatFS, and
// fs.SubFS. Opened files implement io.ReaderAt and io.Seeker. As io/fs
// expects, Open, Stat, and ReadFile follow symlinks (when p implements
// SymlinkProvider; absolute targets resolve against p's root), while
// ReadDir entries describe the links themselves.
func FS(ctx context.Context, p FileProvider) fs.FS {
	return &ioFS{ctx: ctx, p: p}
}

type ioFS struct {
	ctx context.Context
	p   FileProvider
}

var (
	_ fs.ReadDirFS  = (*ioFS)(nil)
	_ fs.ReadFileFS = (*ioFS)(nil)
	_ fs.StatFS     = (*ioFS)(nil)
	_ fs.SubFS      = (*ioFS)(nil)
)

// logical converts an io/fs name to a provider path, validating it.
func logical(op, name string) (string, error) {
	if !fs.ValidPath(name) {
		return "", &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	if name == "." {
		return "/", nil
	}
	return "/" + name, nil
}

// fsName converts a provider path back to an io/fs name.
func fsName(path string) string {
	if path == "/" {
		return "."
	}
	return strings.TrimPrefix(path, "/")
}

// fsErr rewrites a provider *PathError to carry the io/fs name.
func fsErr(op, name string, err error) error {
	var pe *PathError
	if errors.As(err, &pe) {
		return &fs.PathError{Op: op, Path: name, Err: pe.Err}
	}
	return &fs.PathError{Op: op, Path: name, Err: err}
}

// resolve returns the provider path and metadata of p with every symlink
// followed (the final element too).
func (f *ioFS) resolve(p string) (string, FileInfo, error) {
	// Fast path: one Stat when no component is a symlink.
	info, err := f.p.Stat(f.ctx, p)
	if err == nil && !info.Type.IsSymlink() {
		return p, info, nil
	}
	sp, ok := As[SymlinkProvider](f.p)
	if !ok {
		return p, info, err
	}

	hops := 0
	segs := splitPath(p)
	cur := "/"
	for len(segs) > 0 {
		next := JoinPath(cur, segs[0])
		segs = segs[1:]
		info, err := f.p.Stat(f.ctx, next)
		if err != nil {
			return "", FileInfo{}, err
		}
		if !info.Type.IsSymlink() {
			cur = next
			continue
		}
		if hops++; hops > maxSymlinkHops {
			return "", FileInfo{}, &PathError{Op: "resolve", Path: p, Err: errors.New("too many levels of symbolic links")}
		}
		target, err := sp.Readlink(f.ctx, next)
		if err != nil {
			return "", FileInfo{}, err
		}
		// Relative targets resolve against the link's directory, absolute
		// ones against the provider root; restart from the root with the
		// target spliced in front of the remaining components.
		base := cur
		if strings.HasPrefix(target, "/") {
			base = "/"
		}
		segs = append(splitPath(JoinPath(base, target)), segs...)
		cur = "/"
	}
	info, err = f.p.Stat(f.ctx, cur)
	return cur, info, err
}

// splitPath splits a path into its clean, non-empty elements.
func splitPath(p string) []string {
	p = CleanPath(p)
	if p == "/" {
		return nil
	}
	return strings.Split(p[1:], "/")
}

func (f *ioFS) Open(name string) (fs.File, error) {
	p, err := logical("open", name)
	if err != nil {
		return nil, err
	}
	rp, info, err := f.resolve(p)
	if err != nil {
		return nil, fsErr("open", name, err)
	}
	if info.IsDir() {
		return &ioDir{f: f, name: name, path: rp, info: info}, nil
	}
	h, err := f.p.Open(f.ctx, rp, OpenOptions{Read: true})
	if err != nil {
		return nil, fsErr("open", name, err)
	}
	return &ioFile{ctx: f.ctx, h: h, name: name, info: info}, nil
}

func (f *ioFS) Stat(name string) (fs.FileInfo, error) {
	p, err := logical("stat", name)
	if err != nil {
		return nil, err
	}
	_, info, err := f.resolve(p)
	if err != nil {
		return nil, fsErr("stat", name, err)
	}
	return fileInfo{info, name}, nil
}

func (f *ioFS) ReadFile(name string) ([]byte, error) {
	p, err := logical("readfile", name)
	if err != nil {
		return nil, err
	}
	rp, info, err := f.resolve(p)
	if err != nil {
		return nil, fsErr("readfile", name, err)
	}
	if info.IsDir() {
		return nil, &fs.PathError{Op: "readfile", Path: name, Err: ErrIsDir}
	}
	data, err := ReadFile(f.ctx, f.p, rp)
	if err != nil {
		return nil, fsErr("readfile", name, err)
	}
	return data, nil
}

func (f *ioFS) ReadDir(name string) ([]fs.DirEntry, error) {
	p, err := logical("readdir", name)
	if err != nil {
		return nil, err
	}
	rp, info, err := f.resolve(p)
	if err != nil {
		return nil, fsErr("readdir", name, err)
	}
	if !info.IsDir() {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: ErrNotDir}
	}
	entries, err := f.readDir(rp)
	if err != nil {
		return nil, fsErr("readdir", name, err)
	}
	return entries, nil
}

// readDir lists the provider directory p as sorted fs.DirEntries.
func (f *ioFS) readDir(p string) ([]fs.DirEntry, error) {
	var out []fs.DirEntry
	for e, err := range Entries(f.ctx, f.p, p) {
		if err != nil {
			return nil, err
		}
		out = append(out, dirEntry{f: f, dir: p, e: e})
	}
	slices.SortFunc(out, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return out, nil
}

func (f *ioFS) Sub(dir string) (fs.FS, error) {
	p, err := logical("sub", dir)
	if err != nil {
		return nil, err
	}
	rp, info, err := f.resolve(p)
	if err != nil {
		return nil, fsErr("sub", dir, err)
	}
	if !info.IsDir() {
		return nil, &fs.PathError{Op: "sub", Path: dir, Err: ErrNotDir}
	}
	return &ioFS{ctx: f.ctx, p: Sub(f.p, rp)}, nil
}

// fileInfo adapts FileInfo to fs.FileInfo. Sys returns the FileInfo.
type fileInfo struct {
	fi   FileInfo
	name string // io/fs name; base name is derived from it
}

func (i fileInfo) Name() string {
	if i.name == "." {
		return "."
	}
	return i.fi.Name()
}
func (i fileInfo) Size() int64        { return i.fi.Size }
func (i fileInfo) ModTime() time.Time { return i.fi.ModTime }
func (i fileInfo) IsDir() bool        { return i.fi.IsDir() }
func (i fileInfo) Sys() any           { return i.fi }
func (i fileInfo) Mode() fs.FileMode  { return i.fi.Mode.Perm() | typeMode(i.fi.Type) }

func typeMode(t FileType) fs.FileMode {
	switch t {
	case TypeDir:
		return fs.ModeDir
	case TypeSymlink:
		return fs.ModeSymlink
	default:
		return 0
	}
}

// dirEntry adapts DirEntry to fs.DirEntry.
type dirEntry struct {
	f   *ioFS
	dir string
	e   DirEntry
}

func (d dirEntry) Name() string      { return d.e.Name }
func (d dirEntry) IsDir() bool       { return d.e.Type.IsDir() }
func (d dirEntry) Type() fs.FileMode { return typeMode(d.e.Type) }
func (d dirEntry) String() string    { return fs.FormatDirEntry(d) }

func (d dirEntry) Info() (fs.FileInfo, error) {
	if d.e.Info != nil {
		return fileInfo{*d.e.Info, d.e.Name}, nil
	}
	p := JoinPath(d.dir, d.e.Name)
	info, err := d.f.p.Stat(d.f.ctx, p)
	if err != nil {
		return nil, fsErr("stat", fsName(p), err)
	}
	return fileInfo{info, d.e.Name}, nil
}

// ioFile is an open regular file.
type ioFile struct {
	ctx  context.Context
	h    FileHandle
	name string
	info FileInfo

	mu  sync.Mutex
	off int64
}

var (
	_ io.ReaderAt = (*ioFile)(nil)
	_ io.Seeker   = (*ioFile)(nil)
)

func (f *ioFile) Stat() (fs.FileInfo, error) {
	info, err := f.h.Stat(f.ctx)
	if err != nil {
		return nil, fsErr("stat", f.name, err)
	}
	return fileInfo{info, f.name}, nil
}

func (f *ioFile) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	n, err := f.h.ReadAt(p, f.off)
	f.off += int64(n)
	if err == io.EOF && n > 0 {
		err = nil // io.Reader: report EOF on the next call
	}
	return n, err
}

func (f *ioFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, &fs.PathError{Op: "readat", Path: f.name, Err: fs.ErrInvalid}
	}
	return f.h.ReadAt(p, off)
}

func (f *ioFile) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += f.off
	case io.SeekEnd:
		info, err := f.h.Stat(f.ctx)
		if err != nil {
			return 0, err
		}
		offset += info.Size
	default:
		return 0, &fs.PathError{Op: "seek", Path: f.name, Err: fs.ErrInvalid}
	}
	if offset < 0 {
		return 0, &fs.PathError{Op: "seek", Path: f.name, Err: fs.ErrInvalid}
	}
	f.off = offset
	return offset, nil
}

func (f *ioFile) Close() error { return f.h.Close() }

// ioDir is an open directory.
type ioDir struct {
	f    *ioFS
	name string
	path string
	info FileInfo

	entries []fs.DirEntry // loaded on first ReadDir
	loaded  bool
	pos     int
	closed  bool
}

func (d *ioDir) Stat() (fs.FileInfo, error) { return fileInfo{d.info, d.name}, nil }

func (d *ioDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: ErrIsDir}
}

func (d *ioDir) Close() error {
	if d.closed {
		return &fs.PathError{Op: "close", Path: d.name, Err: fs.ErrClosed}
	}
	d.closed = true
	return nil
}

func (d *ioDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.closed {
		return nil, &fs.PathError{Op: "readdir", Path: d.name, Err: fs.ErrClosed}
	}
	if !d.loaded {
		entries, err := d.f.readDir(d.path)
		if err != nil {
			return nil, fsErr("readdir", d.name, err)
		}
		d.entries, d.loaded = entries, true
	}
	rest := d.entries[d.pos:]
	if n <= 0 {
		d.pos = len(d.entries)
		return slices.Clone(rest), nil
	}
	if len(rest) == 0 {
		return nil, io.EOF
	}
	n = min(n, len(rest))
	d.pos += n
	return slices.Clone(rest[:n]), nil
}

// FromFS returns a read-only provider serving fsys — an embed.FS, a
// zip.Reader, os.DirFS, fstest.MapFS, ... — so it can be served over NFS or
// mounted. Mutations fail with ErrReadOnly. Reads use io.ReaderAt when the
// opened file has it, otherwise io.Seeker, otherwise the file is buffered in
// memory on first read. If fsys implements fs.ReadLinkFS, Stat does not
// follow symlinks and the provider implements SymlinkProvider.
func FromFS(fsys fs.FS) FileProvider { return fromFS{fsys} }

type fromFS struct{ fsys fs.FS }

var (
	_ SymlinkProvider = fromFS{}
	_ StatFSProvider  = fromFS{}
)

// name converts a provider path into an fsys name.
func (fromFS) name(path string) string { return fsName(CleanPath(path)) }

// pathErr rewrites an fsys error to carry the logical path.
func (fromFS) pathErr(op, path string, err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return &PathError{Op: op, Path: CleanPath(path), Err: pe.Err}
	}
	return &PathError{Op: op, Path: CleanPath(path), Err: err}
}

func infoFromFS(path string, fi fs.FileInfo) FileInfo {
	t := TypeFile
	switch {
	case fi.IsDir():
		t = TypeDir
	case fi.Mode()&fs.ModeSymlink != 0:
		t = TypeSymlink
	}
	return FileInfo{
		Path:    CleanPath(path),
		Type:    t,
		Size:    fi.Size(),
		Mode:    fi.Mode().Perm(),
		ModTime: fi.ModTime(),
	}
}

func (f fromFS) Stat(ctx context.Context, path string) (FileInfo, error) {
	var fi fs.FileInfo
	var err error
	if rl, ok := f.fsys.(fs.ReadLinkFS); ok {
		fi, err = rl.Lstat(f.name(path))
	} else {
		fi, err = fs.Stat(f.fsys, f.name(path))
	}
	if err != nil {
		return FileInfo{}, f.pathErr("stat", path, err)
	}
	return infoFromFS(path, fi), nil
}

func (f fromFS) Open(ctx context.Context, path string, opts OpenOptions) (FileHandle, error) {
	if opts.Write || opts.Create || opts.Truncate || opts.Append {
		return nil, roErr("open", CleanPath(path))
	}
	file, err := f.fsys.Open(f.name(path))
	if err != nil {
		return nil, f.pathErr("open", path, err)
	}
	fi, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, f.pathErr("open", path, err)
	}
	if fi.IsDir() {
		file.Close()
		return nil, &PathError{Op: "open", Path: CleanPath(path), Err: ErrIsDir}
	}
	return &fromHandle{f: file, info: infoFromFS(path, fi)}, nil
}

func (f fromFS) ReadDir(ctx context.Context, path string, page Page) (DirPage, error) {
	entries, err := fs.ReadDir(f.fsys, f.name(path)) // sorted by name
	if err != nil {
		return DirPage{}, f.pathErr("readdir", path, err)
	}
	if page.Token != "" {
		i, found := slices.BinarySearchFunc(entries, page.Token, func(e fs.DirEntry, tok string) int {
			return strings.Compare(e.Name(), tok)
		})
		if found {
			i++
		}
		entries = entries[i:]
	}
	limit := len(entries)
	if page.Limit > 0 && page.Limit < limit {
		limit = page.Limit
	}
	out := DirPage{Entries: make([]DirEntry, 0, limit)}
	for _, e := range entries[:limit] {
		de := DirEntry{Name: e.Name(), Type: TypeFile}
		switch {
		case e.IsDir():
			de.Type = TypeDir
		case e.Type()&fs.ModeSymlink != 0:
			de.Type = TypeSymlink
		}
		if fi, err := e.Info(); err == nil {
			info := infoFromFS(JoinPath(path, e.Name()), fi)
			de.Info = &info
		}
		out.Entries = append(out.Entries, de)
	}
	if limit < len(entries) {
		out.NextToken = entries[limit-1].Name()
	}
	return out, nil
}

func (f fromFS) Mkdir(ctx context.Context, path string, opts MkdirOptions) error {
	return roErr("mkdir", CleanPath(path))
}

func (f fromFS) Remove(ctx context.Context, path string, opts RemoveOptions) error {
	return roErr("remove", CleanPath(path))
}

func (f fromFS) Rename(ctx context.Context, oldPath, newPath string, opts RenameOptions) error {
	return roErr("rename", CleanPath(oldPath))
}

func (f fromFS) Readlink(ctx context.Context, path string) (string, error) {
	rl, ok := f.fsys.(fs.ReadLinkFS)
	if !ok {
		return "", unsupported("readlink", CleanPath(path))
	}
	target, err := rl.ReadLink(f.name(path))
	if err != nil {
		return "", f.pathErr("readlink", path, err)
	}
	return target, nil
}

func (f fromFS) Symlink(ctx context.Context, target, linkPath string) error {
	return roErr("symlink", CleanPath(linkPath))
}

func (f fromFS) StatFS(ctx context.Context) (FSInfo, error) {
	return FSInfo{ReadOnly: true, MaxNameLength: MaxNameLength}, nil
}

// fromHandle is a read-only FileHandle over an fs.File.
type fromHandle struct {
	f    fs.File
	info FileInfo

	mu     sync.Mutex
	buf    []byte // whole content, for files with neither ReaderAt nor Seeker
	loaded bool
	closed bool
}

func (h *fromHandle) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, &PathError{Op: "read", Path: h.info.Path, Err: ErrInvalid}
	}
	if ra, ok := h.f.(io.ReaderAt); ok {
		return ra.ReadAt(p, off)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, &PathError{Op: "read", Path: h.info.Path, Err: ErrClosed}
	}
	if s, ok := h.f.(io.Seeker); ok {
		if _, err := s.Seek(off, io.SeekStart); err != nil {
			return 0, err
		}
		n, err := io.ReadFull(h.f, p)
		if err == io.ErrUnexpectedEOF {
			err = io.EOF
		}
		return n, err
	}
	if !h.loaded {
		data, err := io.ReadAll(h.f)
		if err != nil {
			return 0, err
		}
		h.buf, h.loaded = data, true
	}
	if off >= int64(len(h.buf)) {
		return 0, io.EOF
	}
	n := copy(p, h.buf[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (h *fromHandle) WriteAt(p []byte, off int64) (int, error) {
	return 0, roErr("write", h.info.Path)
}

func (h *fromHandle) Stat(ctx context.Context) (FileInfo, error) { return h.info, nil }

func (h *fromHandle) Sync(ctx context.Context) error { return nil }

func (h *fromHandle) Truncate(ctx context.Context, size int64) error {
	return roErr("truncate", h.info.Path)
}

func (h *fromHandle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return &PathError{Op: "close", Path: h.info.Path, Err: ErrClosed}
	}
	h.closed = true
	return h.f.Close()
}
