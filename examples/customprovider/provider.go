package main

import (
	"bytes"
	"context"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"ella.to/simplefs"
)

// sysFS is a read-only provider whose files are generated when read:
//
//	/time.txt     the current UTC time
//	/env/<NAME>   the value of each environment variable
//
// It implements just the six FileProvider methods (plus StatFS to advertise
// that it is read-only), which is all a backend needs.
type sysFS struct {
	start time.Time
}

var (
	_ simplefs.FileProvider   = (*sysFS)(nil)
	_ simplefs.StatFSProvider = (*sysFS)(nil)
)

func newSysFS() *sysFS { return &sysFS{start: time.Now()} }

// content returns the bytes of a virtual file, or false if path is not one.
func (s *sysFS) content(path string) ([]byte, bool) {
	if path == "/time.txt" {
		// Fixed-width format: the size reported by Stat stays valid until
		// the file is read, even as the clock ticks.
		return []byte(time.Now().UTC().Format("2006-01-02T15:04:05.000Z") + "\n"), true
	}
	if name, ok := strings.CutPrefix(path, "/env/"); ok && simplefs.ValidName(name) == nil {
		if v, ok := os.LookupEnv(name); ok {
			return []byte(v + "\n"), true
		}
	}
	return nil, false
}

func (s *sysFS) dirInfo(path string) simplefs.FileInfo {
	return simplefs.FileInfo{Path: path, Type: simplefs.TypeDir, Mode: 0o555, ModTime: s.start}
}

func fileInfo(path string, data []byte) simplefs.FileInfo {
	return simplefs.FileInfo{Path: path, Type: simplefs.TypeFile, Size: int64(len(data)), Mode: 0o444, ModTime: time.Now()}
}

func notExist(op, path string) error {
	return &simplefs.PathError{Op: op, Path: path, Err: simplefs.ErrNotExist}
}

func readOnly(op, path string) error {
	return &simplefs.PathError{Op: op, Path: path, Err: simplefs.ErrReadOnly}
}

func (s *sysFS) Stat(ctx context.Context, path string) (simplefs.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return simplefs.FileInfo{}, err
	}
	p := simplefs.CleanPath(path) // every method must accept unclean paths
	if p == "/" || p == "/env" {
		return s.dirInfo(p), nil
	}
	if data, ok := s.content(p); ok {
		return fileInfo(p, data), nil
	}
	return simplefs.FileInfo{}, notExist("stat", p)
}

func (s *sysFS) Open(ctx context.Context, path string, opts simplefs.OpenOptions) (simplefs.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := simplefs.CleanPath(path)
	if opts.Write || opts.Create || opts.Truncate || opts.Append {
		return nil, readOnly("open", p)
	}
	if p == "/" || p == "/env" {
		return nil, &simplefs.PathError{Op: "open", Path: p, Err: simplefs.ErrIsDir}
	}
	data, ok := s.content(p)
	if !ok {
		return nil, notExist("open", p)
	}
	// The content is generated once per open, so a reader sees one
	// consistent snapshot however it reads.
	return &handle{r: bytes.NewReader(data), info: fileInfo(p, data)}, nil
}

func (s *sysFS) ReadDir(ctx context.Context, path string, page simplefs.Page) (simplefs.DirPage, error) {
	if err := ctx.Err(); err != nil {
		return simplefs.DirPage{}, err
	}
	p := simplefs.CleanPath(path)
	var entries []simplefs.DirEntry
	switch p {
	case "/":
		envDir := s.dirInfo("/env")
		data, _ := s.content("/time.txt")
		timeInfo := fileInfo("/time.txt", data)
		entries = []simplefs.DirEntry{
			{Name: "env", Type: simplefs.TypeDir, Info: &envDir},
			{Name: "time.txt", Type: simplefs.TypeFile, Info: &timeInfo},
		}
	case "/env":
		for _, kv := range os.Environ() {
			name, value, _ := strings.Cut(kv, "=")
			if simplefs.ValidName(name) != nil {
				continue // e.g. Windows' "=C:" pseudo variables
			}
			info := fileInfo("/env/"+name, []byte(value+"\n"))
			entries = append(entries, simplefs.DirEntry{Name: name, Type: simplefs.TypeFile, Info: &info})
		}
		slices.SortFunc(entries, func(a, b simplefs.DirEntry) int { return strings.Compare(a.Name, b.Name) })
		entries = slices.CompactFunc(entries, func(a, b simplefs.DirEntry) bool { return a.Name == b.Name })
	default:
		if _, ok := s.content(p); ok {
			return simplefs.DirPage{}, &simplefs.PathError{Op: "readdir", Path: p, Err: simplefs.ErrNotDir}
		}
		return simplefs.DirPage{}, notExist("readdir", p)
	}

	// Pagination: entries are sorted, so the token is simply the last name
	// returned — stable even if the list changes between pages.
	if page.Token != "" {
		i, found := slices.BinarySearchFunc(entries, page.Token, func(e simplefs.DirEntry, t string) int {
			return strings.Compare(e.Name, t)
		})
		if found {
			i++
		}
		entries = entries[i:]
	}
	var out simplefs.DirPage
	if page.Limit > 0 && page.Limit < len(entries) {
		entries = entries[:page.Limit]
		out.NextToken = entries[len(entries)-1].Name
	}
	out.Entries = entries
	return out, nil
}

func (s *sysFS) Mkdir(ctx context.Context, path string, opts simplefs.MkdirOptions) error {
	return readOnly("mkdir", simplefs.CleanPath(path))
}

func (s *sysFS) Remove(ctx context.Context, path string, opts simplefs.RemoveOptions) error {
	return readOnly("remove", simplefs.CleanPath(path))
}

func (s *sysFS) Rename(ctx context.Context, oldPath, newPath string, opts simplefs.RenameOptions) error {
	return readOnly("rename", simplefs.CleanPath(oldPath))
}

// StatFS tells backends (NFS, WebDAV, mount) that nothing is writable, so
// clients can say so up front instead of failing each write.
func (s *sysFS) StatFS(ctx context.Context) (simplefs.FSInfo, error) {
	return simplefs.FSInfo{ReadOnly: true, MaxNameLength: simplefs.MaxNameLength}, nil
}

// handle serves one generated snapshot.
type handle struct {
	r    *bytes.Reader
	info simplefs.FileInfo

	mu     sync.Mutex
	closed bool
}

func (h *handle) check(op string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return &simplefs.PathError{Op: op, Path: h.info.Path, Err: simplefs.ErrClosed}
	}
	return nil
}

func (h *handle) ReadAt(p []byte, off int64) (int, error) {
	if err := h.check("read"); err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, &simplefs.PathError{Op: "read", Path: h.info.Path, Err: simplefs.ErrInvalid}
	}
	return h.r.ReadAt(p, off) // bytes.Reader already follows the io.ReaderAt EOF rules
}

func (h *handle) WriteAt(p []byte, off int64) (int, error) { return 0, readOnly("write", h.info.Path) }

func (h *handle) Stat(ctx context.Context) (simplefs.FileInfo, error) { return h.info, h.check("stat") }

func (h *handle) Sync(ctx context.Context) error { return h.check("sync") }

func (h *handle) Truncate(ctx context.Context, size int64) error {
	return readOnly("truncate", h.info.Path)
}

func (h *handle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return &simplefs.PathError{Op: "close", Path: h.info.Path, Err: simplefs.ErrClosed}
	}
	h.closed = true
	return nil
}
