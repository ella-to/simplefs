package memfs

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"testing"
	"time"

	"ella.to/simplefs"
)

func writeFile(t *testing.T, m *MemFS, path, content string) {
	t.Helper()
	h, err := m.Create(t.Context(), path, simplefs.CreateOptions{})
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	if _, err := h.WriteAt([]byte(content), 0); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

func readFile(t *testing.T, m *MemFS, path string) string {
	t.Helper()
	h, err := m.Open(t.Context(), path, simplefs.OpenOptions{Read: true})
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer h.Close()
	info, err := h.Stat(t.Context())
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	data := make([]byte, info.Size)
	if _, err := h.ReadAt(data, 0); err != nil && err != io.EOF {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestCreateWriteRead(t *testing.T) {
	m := New()
	writeFile(t, m, "/a.txt", "hello")

	if got := readFile(t, m, "/a.txt"); got != "hello" {
		t.Fatalf("content = %q, want %q", got, "hello")
	}
	info, err := m.Stat(t.Context(), "/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != 5 || !info.Type.IsFile() || info.Name != "a.txt" || info.Path != "/a.txt" {
		t.Fatalf("unexpected info: %+v", info)
	}
}

func TestETagChangesOnWrite(t *testing.T) {
	m := New()
	writeFile(t, m, "/a.txt", "one")
	before, _ := m.Stat(t.Context(), "/a.txt")
	writeFile(t, m, "/a.txt", "two")
	after, _ := m.Stat(t.Context(), "/a.txt")
	if before.ETag == after.ETag {
		t.Fatalf("ETag did not change: %q", before.ETag)
	}
}

func TestOpenFlags(t *testing.T) {
	ctx := t.Context()
	m := New()

	if _, err := m.Open(ctx, "/missing", simplefs.OpenOptions{Read: true}); !errors.Is(err, simplefs.ErrNotExist) {
		t.Fatalf("open missing = %v, want ErrNotExist", err)
	}
	if _, err := m.Open(ctx, "/new", simplefs.OpenOptions{Write: true, Create: true}); err != nil {
		t.Fatalf("open create: %v", err)
	}
	if _, err := m.Open(ctx, "/new", simplefs.OpenOptions{Write: true, Create: true, Exclusive: true}); !errors.Is(err, simplefs.ErrExist) {
		t.Fatalf("open exclusive over existing = %v, want ErrExist", err)
	}

	// Truncate empties the file on open.
	writeFile(t, m, "/t.txt", "content")
	h, err := m.Open(ctx, "/t.txt", simplefs.OpenOptions{Write: true, Truncate: true})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := h.Stat(ctx)
	if info.Size != 0 {
		t.Fatalf("size after truncate = %d, want 0", info.Size)
	}
	h.Close()

	// Append ignores the offset.
	writeFile(t, m, "/app.txt", "ab")
	h, err = m.Open(ctx, "/app.txt", simplefs.OpenOptions{Write: true, Append: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.WriteAt([]byte("cd"), 0); err != nil {
		t.Fatal(err)
	}
	h.Close()
	if got := readFile(t, m, "/app.txt"); got != "abcd" {
		t.Fatalf("append result = %q, want %q", got, "abcd")
	}

	// A read-only handle refuses writes; a write-only handle refuses reads.
	h, _ = m.Open(ctx, "/app.txt", simplefs.OpenOptions{Read: true})
	if _, err := h.WriteAt([]byte("x"), 0); !errors.Is(err, simplefs.ErrPermission) {
		t.Fatalf("write on read-only handle = %v, want ErrPermission", err)
	}
	h.Close()
	h, _ = m.Open(ctx, "/app.txt", simplefs.OpenOptions{Write: true})
	if _, err := h.ReadAt(make([]byte, 1), 0); !errors.Is(err, simplefs.ErrPermission) {
		t.Fatalf("read on write-only handle = %v, want ErrPermission", err)
	}
	h.Close()
	if _, err := h.ReadAt(make([]byte, 1), 0); !errors.Is(err, simplefs.ErrClosed) {
		t.Fatalf("read after close = %v, want ErrClosed", err)
	}
}

func TestMkdirReadDirPagination(t *testing.T) {
	ctx := t.Context()
	m := New()
	if err := m.Mkdir(ctx, "/dir", simplefs.MkdirOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := m.Mkdir(ctx, "/dir", simplefs.MkdirOptions{}); !errors.Is(err, simplefs.ErrExist) {
		t.Fatalf("mkdir existing = %v, want ErrExist", err)
	}
	if err := m.Mkdir(ctx, "/no/parent", simplefs.MkdirOptions{}); !errors.Is(err, simplefs.ErrNotExist) {
		t.Fatalf("mkdir without parent = %v, want ErrNotExist", err)
	}

	for _, name := range []string{"a", "b", "c", "d", "e"} {
		writeFile(t, m, "/dir/"+name, name)
	}

	var got []string
	page := simplefs.Page{Limit: 2}
	pages := 0
	for {
		dp, err := m.ReadDir(ctx, "/dir", page)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, e := range dp.Entries {
			got = append(got, e.Name)
			if e.Info == nil {
				t.Fatalf("entry %s has nil Info", e.Name)
			}
		}
		if dp.NextToken == "" {
			break
		}
		page.Token = dp.NextToken
	}
	if pages != 3 || len(got) != 5 {
		t.Fatalf("pages=%d entries=%v", pages, got)
	}
	for i, want := range []string{"a", "b", "c", "d", "e"} {
		if got[i] != want {
			t.Fatalf("entries = %v, want sorted a..e", got)
		}
	}
}

func TestRemove(t *testing.T) {
	ctx := t.Context()
	m := New()
	if err := m.Mkdir(ctx, "/dir", simplefs.MkdirOptions{}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, m, "/dir/f", "x")

	if err := m.Remove(ctx, "/dir", simplefs.RemoveOptions{}); !errors.Is(err, simplefs.ErrNotEmpty) {
		t.Fatalf("remove non-empty = %v, want ErrNotEmpty", err)
	}
	if err := m.Remove(ctx, "/dir", simplefs.RemoveOptions{Recursive: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(ctx, "/dir", simplefs.RemoveOptions{}); !errors.Is(err, simplefs.ErrNotExist) {
		t.Fatalf("remove missing = %v, want ErrNotExist", err)
	}
	if err := m.Remove(ctx, "/dir", simplefs.RemoveOptions{Force: true}); err != nil {
		t.Fatalf("forced remove missing = %v, want nil", err)
	}
	if err := m.Remove(ctx, "/", simplefs.RemoveOptions{Recursive: true}); !errors.Is(err, simplefs.ErrInvalid) {
		t.Fatalf("remove root = %v, want ErrInvalid", err)
	}
}

func TestRename(t *testing.T) {
	ctx := t.Context()
	m := New()
	writeFile(t, m, "/a", "content")
	writeFile(t, m, "/b", "other")
	if err := m.Mkdir(ctx, "/dir", simplefs.MkdirOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := m.Rename(ctx, "/a", "/b", simplefs.RenameOptions{}); !errors.Is(err, simplefs.ErrExist) {
		t.Fatalf("rename onto existing = %v, want ErrExist", err)
	}
	if err := m.Rename(ctx, "/a", "/b", simplefs.RenameOptions{Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, m, "/b"); got != "content" {
		t.Fatalf("content after overwrite = %q", got)
	}
	if err := m.Rename(ctx, "/b", "/dir/b", simplefs.RenameOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stat(ctx, "/dir/b"); err != nil {
		t.Fatalf("moved file missing: %v", err)
	}
	if err := m.Rename(ctx, "/dir", "/dir/sub", simplefs.RenameOptions{}); !errors.Is(err, simplefs.ErrInvalid) {
		t.Fatalf("rename into own subtree = %v, want ErrInvalid", err)
	}
}

func TestSymlinkAndHardLink(t *testing.T) {
	ctx := t.Context()
	m := New()
	writeFile(t, m, "/target", "data")

	if err := m.Symlink(ctx, "/target", "/link"); err != nil {
		t.Fatal(err)
	}
	got, err := m.Readlink(ctx, "/link")
	if err != nil || got != "/target" {
		t.Fatalf("readlink = %q, %v", got, err)
	}
	info, _ := m.Stat(ctx, "/link")
	if !info.Type.IsSymlink() {
		t.Fatalf("link type = %v, want symlink", info.Type)
	}

	if err := m.Link(ctx, "/target", "/hard"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, m, "/target", "changed")
	if got := readFile(t, m, "/hard"); got != "changed" {
		t.Fatalf("hard link content = %q, want %q (shared node)", got, "changed")
	}
}

func TestMetaCapabilities(t *testing.T) {
	ctx := t.Context()
	m := New()
	writeFile(t, m, "/f", "x")

	if err := m.Chmod(ctx, "/f", 0o600); err != nil {
		t.Fatal(err)
	}
	if info, _ := m.Stat(ctx, "/f"); info.Mode != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode)
	}
	if err := m.Chown(ctx, "/f", 501, 20); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := m.Chtimes(ctx, "/f", when, when); err != nil {
		t.Fatal(err)
	}
	if info, _ := m.Stat(ctx, "/f"); !info.ModTime.Equal(when) {
		t.Fatalf("mtime = %v, want %v", info.ModTime, when)
	}

	if err := m.SetXattr(ctx, "/f", "user.k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	v, err := m.GetXattr(ctx, "/f", "user.k")
	if err != nil || string(v) != "v" {
		t.Fatalf("getxattr = %q, %v", v, err)
	}
	names, _ := m.ListXattr(ctx, "/f")
	if len(names) != 1 || names[0] != "user.k" {
		t.Fatalf("listxattr = %v", names)
	}
	if err := m.RemoveXattr(ctx, "/f", "user.k"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GetXattr(ctx, "/f", "user.k"); !errors.Is(err, simplefs.ErrNotExist) {
		t.Fatalf("getxattr removed = %v, want ErrNotExist", err)
	}

	fsinfo, err := m.StatFS(ctx)
	if err != nil || fsinfo.Blocks == 0 || fsinfo.BlockSize == 0 {
		t.Fatalf("statfs = %+v, %v", fsinfo, err)
	}
}

func TestWatch(t *testing.T) {
	m := New()
	ctx, cancel := context.WithCancel(t.Context())

	events, err := m.Watch(ctx, "/", true)
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, m, "/w.txt", "1")
	if err := m.Rename(t.Context(), "/w.txt", "/w2.txt", simplefs.RenameOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(t.Context(), "/w2.txt", simplefs.RemoveOptions{}); err != nil {
		t.Fatal(err)
	}

	want := []simplefs.EventType{simplefs.EventCreate, simplefs.EventModify, simplefs.EventRename, simplefs.EventDelete}
	for i, wt := range want {
		select {
		case ev := <-events:
			if ev.Type != wt {
				t.Fatalf("event %d = %v (%s), want %v", i, ev.Type, ev.Path, wt)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for event %d (%v)", i, wt)
		}
	}

	cancel()
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("expected channel to close after cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("channel not closed after cancel")
	}
}

func TestWatchScope(t *testing.T) {
	m := New()
	ctx := t.Context()
	if err := m.Mkdir(ctx, "/dir", simplefs.MkdirOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := m.Mkdir(ctx, "/dir/sub", simplefs.MkdirOptions{}); err != nil {
		t.Fatal(err)
	}

	flat, err := m.Watch(ctx, "/dir", false)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, m, "/dir/sub/deep.txt", "x") // create+modify, below the non-recursive scope
	writeFile(t, m, "/dir/direct.txt", "x")   // create+modify, direct child

	ev := <-flat
	if ev.Path != "/dir/direct.txt" || ev.Type != simplefs.EventCreate {
		t.Fatalf("non-recursive watcher got %v %s, want create /dir/direct.txt", ev.Type, ev.Path)
	}
}

func TestShrinkThenGrowReadsZeros(t *testing.T) {
	ctx := t.Context()
	m := New()
	h, err := m.Create(ctx, "/f", simplefs.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.WriteAt([]byte("abcdefgh"), 0)
	if err := h.Truncate(ctx, 2); err != nil {
		t.Fatal(err)
	}
	h.WriteAt([]byte("Z"), 6) // grows within the old capacity
	buf := make([]byte, 7)
	n, _ := h.ReadAt(buf, 0)
	if got := string(buf[:n]); got != "ab\x00\x00\x00\x00Z" {
		t.Fatalf("content = %q", got)
	}
}

func TestStatFSTracksUsage(t *testing.T) {
	ctx := t.Context()
	m := New()
	used := func() int64 {
		t.Helper()
		if _, err := m.StatFS(ctx); err != nil {
			t.Fatal(err)
		}
		return m.usedBytes
	}
	write := func(path string, n int) {
		h, err := m.Create(ctx, path, simplefs.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		h.WriteAt(make([]byte, n), 0)
		h.Close()
	}
	m.Mkdir(ctx, "/d", simplefs.MkdirOptions{})
	write("/d/a", 100)
	write("/b", 50)
	if got := used(); got != 150 {
		t.Fatalf("used = %d, want 150", got)
	}
	m.Link(ctx, "/b", "/b2") // a hard link is not new content
	if got := used(); got != 150 {
		t.Fatalf("used after link = %d, want 150", got)
	}
	m.Remove(ctx, "/b", simplefs.RemoveOptions{})
	if got := used(); got != 150 {
		t.Fatalf("used after removing one link = %d, want 150", got)
	}
	write("/c", 10)
	m.Rename(ctx, "/c", "/b2", simplefs.RenameOptions{Overwrite: true})
	if got := used(); got != 110 {
		t.Fatalf("used after overwrite = %d, want 110", got)
	}
	m.Remove(ctx, "/d", simplefs.RemoveOptions{Recursive: true})
	if got := used(); got != 10 {
		t.Fatalf("used after recursive remove = %d, want 10", got)
	}
}

func TestHandleStatFollowsRename(t *testing.T) {
	ctx := t.Context()
	m := New()
	m.Mkdir(ctx, "/a", simplefs.MkdirOptions{})
	h, err := m.Create(ctx, "/a/f", simplefs.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err := m.Rename(ctx, "/a", "/b", simplefs.RenameOptions{}); err != nil {
		t.Fatal(err)
	}
	info, err := h.Stat(ctx)
	if err != nil || info.Path != "/b/f" || info.Name != "f" {
		t.Fatalf("Stat after rename = %+v, %v; want /b/f", info, err)
	}
}

func TestCapacity(t *testing.T) {
	ctx := t.Context()
	m := New(WithCapacity(100))
	h, err := m.Create(ctx, "/f", simplefs.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.WriteAt(make([]byte, 100), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := h.WriteAt([]byte{1}, 100); !errors.Is(err, simplefs.ErrNoSpace) {
		t.Fatalf("write past capacity = %v, want ErrNoSpace", err)
	}
	if err := h.Truncate(ctx, 1<<62); !errors.Is(err, simplefs.ErrNoSpace) {
		t.Fatalf("huge truncate = %v, want ErrNoSpace", err)
	}
	if _, err := h.WriteAt([]byte{1}, 1<<63-1); !errors.Is(err, simplefs.ErrNoSpace) {
		t.Fatalf("overflowing write = %v, want ErrNoSpace", err)
	}
	fsinfo, _ := m.StatFS(ctx)
	if fsinfo.Blocks*fsinfo.BlockSize > 100 {
		t.Fatalf("StatFS reports %d bytes, capacity is 100", fsinfo.Blocks*fsinfo.BlockSize)
	}
}

func TestErrorsMatchIOFS(t *testing.T) {
	_, err := New().Stat(t.Context(), "/missing")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat missing = %v, want fs.ErrNotExist", err)
	}
	var pe *fs.PathError
	if !errors.As(err, &pe) || pe.Path != "/missing" {
		t.Fatalf("error %v is not an *fs.PathError for /missing", err)
	}
}
