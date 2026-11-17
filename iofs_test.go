package simplefs_test

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"testing/fstest"

	"ella.to/simplefs"
	"ella.to/simplefs/memfs"
)

func seeded(t *testing.T) *memfs.MemFS {
	t.Helper()
	ctx := t.Context()
	p := memfs.New()
	simplefs.MkdirAll(ctx, p, "/a/b", 0)
	simplefs.MkdirAll(ctx, p, "/empty", 0)
	simplefs.WriteFile(ctx, p, "/top.txt", []byte("top"), 0)
	simplefs.WriteFile(ctx, p, "/a/one.txt", []byte("one"), 0)
	simplefs.WriteFile(ctx, p, "/a/b/two.txt", []byte("two two"), 0)
	return p
}

func TestFSConformance(t *testing.T) {
	p := seeded(t)
	if err := fstest.TestFS(simplefs.FS(t.Context(), p), "top.txt", "a/one.txt", "a/b/two.txt", "empty"); err != nil {
		t.Fatal(err)
	}
}

func TestFSFollowsSymlinks(t *testing.T) {
	ctx := t.Context()
	p := seeded(t)
	p.Symlink(ctx, "b/two.txt", "/a/rel")    // relative to /a
	p.Symlink(ctx, "/a/b", "/abs")           // absolute, a directory
	p.Symlink(ctx, "/abs/two.txt", "/chain") // through another link
	p.Symlink(ctx, "/loop2", "/loop1")
	p.Symlink(ctx, "/loop1", "/loop2")
	fsys := simplefs.FS(ctx, p)

	for _, name := range []string{"a/rel", "abs/two.txt", "chain"} {
		data, err := fs.ReadFile(fsys, name)
		if err != nil || string(data) != "two two" {
			t.Errorf("ReadFile(%s) = %q, %v", name, data, err)
		}
	}
	if info, err := fs.Stat(fsys, "abs"); err != nil || !info.IsDir() {
		t.Errorf("Stat(abs) = %v, %v; want a directory", info, err)
	}
	if _, err := fs.ReadFile(fsys, "loop1"); err == nil {
		t.Error("symlink loop resolved")
	}
	// ReadDir reports the link itself.
	entries, _ := fs.ReadDir(fsys, ".")
	for _, e := range entries {
		if e.Name() == "abs" && e.Type()&fs.ModeSymlink == 0 {
			t.Errorf("ReadDir entry for a symlink has type %v", e.Type())
		}
	}
	sub, err := fs.Sub(fsys, "abs")
	if err != nil {
		t.Fatal(err)
	}
	if data, err := fs.ReadFile(sub, "two.txt"); err != nil || string(data) != "two two" {
		t.Errorf("Sub through a symlink: %q, %v", data, err)
	}
}

func TestFSFileSeekReadAt(t *testing.T) {
	f, err := simplefs.FS(t.Context(), seeded(t)).Open("a/b/two.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 3)
	if n, err := f.(io.ReaderAt).ReadAt(buf, 4); err != nil && err != io.EOF || string(buf[:n]) != "two" {
		t.Fatalf("ReadAt = %q, %v", buf[:n], err)
	}
	if off, err := f.(io.Seeker).Seek(-3, io.SeekEnd); err != nil || off != 4 {
		t.Fatalf("Seek = %d, %v", off, err)
	}
	rest, _ := io.ReadAll(f)
	if string(rest) != "two" {
		t.Fatalf("read after seek = %q", rest)
	}
}

func TestFromFS(t *testing.T) {
	ctx := t.Context()
	mapfs := fstest.MapFS{
		"hello.txt":     {Data: []byte("hi"), Mode: 0o640},
		"dir/inner.txt": {Data: []byte("inner")},
	}
	p := simplefs.FromFS(mapfs)
	got, err := simplefs.ReadFile(ctx, p, "/dir/inner.txt")
	if err != nil || string(got) != "inner" {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
	info, err := p.Stat(ctx, "/hello.txt")
	if err != nil || info.Mode != 0o640 || info.Path != "/hello.txt" || info.Size != 2 {
		t.Fatalf("Stat = %+v, %v", info, err)
	}
	if err := simplefs.WriteFile(ctx, p, "/new", nil, 0); !errors.Is(err, simplefs.ErrReadOnly) {
		t.Fatalf("write = %v, want ErrReadOnly", err)
	}
	if _, err := p.Stat(ctx, "/missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing = %v", err)
	}
	var names []string
	for e, err := range simplefs.Entries(ctx, p, "/") {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, e.Name)
	}
	if !slices.Equal(names, []string{"dir", "hello.txt"}) {
		t.Fatalf("entries = %v", names)
	}
	page, _ := p.ReadDir(ctx, "/", simplefs.Page{Limit: 1})
	if len(page.Entries) != 1 || page.NextToken != "dir" {
		t.Fatalf("paged ReadDir = %+v", page)
	}
}

func TestFromFSRoundTrip(t *testing.T) {
	p := simplefs.FromFS(simplefs.FS(t.Context(), seeded(t)))
	if err := fstest.TestFS(simplefs.FS(t.Context(), p), "top.txt", "a/b/two.txt"); err != nil {
		t.Fatal(err)
	}
}

func TestFromFSDirFSSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "target"), []byte("t"), 0o644)
	os.Symlink("target", filepath.Join(dir, "link"))
	p := simplefs.FromFS(os.DirFS(dir))
	info, err := p.Stat(t.Context(), "/link")
	if err != nil || !info.Type.IsSymlink() {
		t.Fatalf("Stat(link) = %+v, %v; want a symlink (Lstat)", info, err)
	}
	sp, ok := simplefs.As[simplefs.SymlinkProvider](p)
	if !ok {
		t.Fatal("FromFS(os.DirFS) is not a SymlinkProvider")
	}
	if target, err := sp.Readlink(t.Context(), "/link"); err != nil || target != "target" {
		t.Fatalf("Readlink = %q, %v", target, err)
	}
}
