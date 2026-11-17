package simplefs_test

import (
	"context"
	"errors"
	"io/fs"
	"testing"
	"time"

	"ella.to/simplefs"
	"ella.to/simplefs/memfs"
)

// logging is middleware that forwards everything via Unwrap.
type logging struct {
	simplefs.FileProvider
	calls int
}

func (l *logging) Unwrap() simplefs.FileProvider { return l.FileProvider }

func (l *logging) Stat(ctx context.Context, path string) (simplefs.FileInfo, error) {
	l.calls++
	return l.FileProvider.Stat(ctx, path)
}

func TestAsThroughMiddleware(t *testing.T) {
	inner := memfs.New()
	var p simplefs.FileProvider = &logging{FileProvider: inner}
	sp, ok := simplefs.As[simplefs.SymlinkProvider](p)
	if !ok || sp != simplefs.SymlinkProvider(inner) {
		t.Fatalf("As did not reach the inner SymlinkProvider")
	}
	if _, ok := simplefs.As[simplefs.SymlinkProvider](plain{}); ok {
		t.Fatal("As found a capability on a provider without it")
	}
	if _, ok := simplefs.As[*logging](p); !ok {
		t.Fatal("As could not find the wrapper itself")
	}
}

// plain is a provider with no capabilities.
type plain struct{ simplefs.FileProvider }

func TestReadOnly(t *testing.T) {
	ctx := t.Context()
	inner := memfs.New()
	simplefs.WriteFile(ctx, inner, "/f", []byte("data"), 0)
	ro := simplefs.ReadOnly(inner)

	if got, err := simplefs.ReadFile(ctx, ro, "/f"); err != nil || string(got) != "data" {
		t.Fatalf("read through ReadOnly = %q, %v", got, err)
	}
	mutations := map[string]error{
		"create": simplefs.WriteFile(ctx, ro, "/g", nil, 0),
		"mkdir":  ro.Mkdir(ctx, "/d", simplefs.MkdirOptions{}),
		"remove": ro.Remove(ctx, "/f", simplefs.RemoveOptions{}),
		"rename": ro.Rename(ctx, "/f", "/g", simplefs.RenameOptions{}),
	}
	cp, _ := simplefs.As[simplefs.ChmodProvider](ro)
	mutations["chmod"] = cp.Chmod(ctx, "/f", 0o600)
	sp, _ := simplefs.As[simplefs.SymlinkProvider](ro)
	mutations["symlink"] = sp.Symlink(ctx, "/f", "/l")
	for op, err := range mutations {
		if !errors.Is(err, simplefs.ErrReadOnly) {
			t.Errorf("%s = %v, want ErrReadOnly", op, err)
		}
	}
	h, err := ro.Open(ctx, "/f", simplefs.OpenOptions{Read: true})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.WriteAt([]byte("x"), 0); !errors.Is(err, simplefs.ErrReadOnly) {
		t.Fatalf("handle write = %v", err)
	}
	sfs, _ := simplefs.As[simplefs.StatFSProvider](ro)
	if info, err := sfs.StatFS(ctx); err != nil || !info.ReadOnly {
		t.Fatalf("StatFS = %+v, %v; want ReadOnly", info, err)
	}
	// A capability the inner provider lacks reports ErrNotSupported.
	sp2, _ := simplefs.As[simplefs.SymlinkProvider](simplefs.ReadOnly(plain{inner}))
	if _, err := sp2.Readlink(ctx, "/f"); !errors.Is(err, simplefs.ErrNotSupported) {
		t.Fatalf("Readlink without capability = %v", err)
	}
}

func TestSub(t *testing.T) {
	ctx := t.Context()
	inner := memfs.New()
	simplefs.MkdirAll(ctx, inner, "/a/b", 0)
	simplefs.WriteFile(ctx, inner, "/a/b/f", []byte("x"), 0)
	simplefs.WriteFile(ctx, inner, "/secret", []byte("s"), 0)
	sub := simplefs.Sub(inner, "/a")

	info, err := sub.Stat(ctx, "/b/f")
	if err != nil || info.Path != "/b/f" {
		t.Fatalf("Stat = %+v, %v", info, err)
	}
	if root, err := sub.Stat(ctx, "/"); err != nil || root.Path != "/" || !root.IsDir() {
		t.Fatalf("Stat(/) = %+v, %v", root, err)
	}
	if _, err := sub.Stat(ctx, "/../secret"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("escape via .. = %v, want ErrNotExist", err)
	}
	_, err = sub.Stat(ctx, "/nope")
	var pe *fs.PathError
	if !errors.As(err, &pe) || pe.Path != "/nope" {
		t.Fatalf("error path = %v, want /nope", err)
	}
	page, err := sub.ReadDir(ctx, "/b", simplefs.Page{})
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Info.Path != "/b/f" {
		t.Fatalf("ReadDir = %+v, %v", page, err)
	}
	h, err := sub.Open(ctx, "/b/f", simplefs.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if hi, _ := h.Stat(ctx); hi.Path != "/b/f" {
		t.Fatalf("handle Stat path = %q", hi.Path)
	}
	h.Close()
	if err := sub.Remove(ctx, "/", simplefs.RemoveOptions{Recursive: true}); !errors.Is(err, simplefs.ErrInvalid) {
		t.Fatalf("Remove(/) = %v", err)
	}
	// Nested Sub composes.
	if info, err := simplefs.Sub(sub, "/b").Stat(ctx, "/f"); err != nil || info.Path != "/f" {
		t.Fatalf("nested Sub Stat = %+v, %v", info, err)
	}
}

func TestSubWatch(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	inner := memfs.New()
	simplefs.MkdirAll(ctx, inner, "/a", 0)
	simplefs.WriteFile(ctx, inner, "/outside", nil, 0)
	wp, _ := simplefs.As[simplefs.WatchProvider](simplefs.Sub(inner, "/a"))
	events, err := wp.Watch(ctx, "/", true)
	if err != nil {
		t.Fatal(err)
	}
	simplefs.WriteFile(ctx, inner, "/ignored", nil, 0)
	inner.Rename(ctx, "/outside", "/a/in", simplefs.RenameOptions{})
	inner.Rename(ctx, "/a/in", "/out", simplefs.RenameOptions{})

	want := []simplefs.Event{
		{Type: simplefs.EventCreate, Path: "/in"},
		{Type: simplefs.EventDelete, Path: "/in"},
	}
	for _, w := range want {
		select {
		case ev := <-events:
			if ev.Type != w.Type || ev.Path != w.Path || ev.OldPath != "" {
				t.Fatalf("event = %+v, want %+v", ev, w)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %+v", w)
		}
	}
}
