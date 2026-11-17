package simplefs_test

import (
	"context"
	"errors"
	"io/fs"
	"slices"
	"strconv"
	"testing"

	"ella.to/simplefs"
	"ella.to/simplefs/memfs"
)

func TestReadWriteFile(t *testing.T) {
	ctx := t.Context()
	p := memfs.New()
	if err := simplefs.WriteFile(ctx, p, "/a.txt", []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := simplefs.ReadFile(ctx, p, "/a.txt")
	if err != nil || string(got) != "hello" {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
	info, _ := p.Stat(ctx, "/a.txt")
	if info.Mode != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode)
	}
	// Overwrite truncates.
	simplefs.WriteFile(ctx, p, "/a.txt", []byte("hi"), 0)
	if got, _ := simplefs.ReadFile(ctx, p, "/a.txt"); string(got) != "hi" {
		t.Fatalf("after overwrite = %q", got)
	}
	if _, err := simplefs.ReadFile(ctx, p, "/missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadFile missing = %v", err)
	}
	empty := simplefs.WriteFile(ctx, p, "/empty", nil, 0)
	if got, err := simplefs.ReadFile(ctx, p, "/empty"); empty != nil || err != nil || len(got) != 0 {
		t.Fatalf("empty file = %q, %v, %v", got, err, empty)
	}
}

func TestExistsMkdirAll(t *testing.T) {
	ctx := t.Context()
	p := memfs.New()
	if err := simplefs.MkdirAll(ctx, p, "/a/b/c", 0); err != nil {
		t.Fatal(err)
	}
	if err := simplefs.MkdirAll(ctx, p, "/a/b/c", 0); err != nil {
		t.Fatalf("MkdirAll on existing dir = %v", err)
	}
	if ok, err := simplefs.Exists(ctx, p, "/a/b/c"); !ok || err != nil {
		t.Fatalf("Exists = %v, %v", ok, err)
	}
	if ok, err := simplefs.Exists(ctx, p, "/nope"); ok || err != nil {
		t.Fatalf("Exists(missing) = %v, %v", ok, err)
	}
	simplefs.WriteFile(ctx, p, "/a/file", nil, 0)
	if err := simplefs.MkdirAll(ctx, p, "/a/file/x", 0); !errors.Is(err, simplefs.ErrNotDir) {
		t.Fatalf("MkdirAll through a file = %v, want ErrNotDir", err)
	}
}

// noRecursive refuses recursive removal, forcing RemoveAll's fallback.
type noRecursive struct{ *memfs.MemFS }

func (n noRecursive) Remove(ctx context.Context, path string, opts simplefs.RemoveOptions) error {
	if opts.Recursive {
		return simplefs.ErrNotSupported
	}
	return n.MemFS.Remove(ctx, path, opts)
}

func TestRemoveAll(t *testing.T) {
	ctx := t.Context()
	for name, p := range map[string]simplefs.FileProvider{
		"recursive": memfs.New(),
		"fallback":  noRecursive{memfs.New()},
	} {
		t.Run(name, func(t *testing.T) {
			simplefs.MkdirAll(ctx, p, "/x/y/z", 0)
			for i := range 600 { // more than one ReadDir page
				simplefs.WriteFile(ctx, p, "/x/y/f"+strconv.Itoa(i), []byte("d"), 0)
			}
			if err := simplefs.RemoveAll(ctx, p, "/x"); err != nil {
				t.Fatal(err)
			}
			if ok, _ := simplefs.Exists(ctx, p, "/x"); ok {
				t.Fatal("/x still exists")
			}
			if err := simplefs.RemoveAll(ctx, p, "/x"); err != nil {
				t.Fatalf("RemoveAll(missing) = %v", err)
			}
		})
	}
}

func TestEntriesPaginates(t *testing.T) {
	ctx := t.Context()
	p := memfs.New()
	var want []string
	for i := range 1200 {
		name := "f" + strconv.Itoa(i)
		want = append(want, name)
		simplefs.WriteFile(ctx, p, "/"+name, nil, 0)
	}
	var got []string
	for e, err := range simplefs.Entries(ctx, p, "/") {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, e.Name)
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for _, err := range simplefs.Entries(ctx, p, "/f1") {
		if !errors.Is(err, simplefs.ErrNotDir) {
			t.Fatalf("Entries(file) err = %v", err)
		}
	}
	// Early break must not panic.
	for range simplefs.Entries(ctx, p, "/") {
		break
	}
}

func TestWalkDir(t *testing.T) {
	ctx := t.Context()
	p := memfs.New()
	simplefs.MkdirAll(ctx, p, "/a/b", 0)
	simplefs.MkdirAll(ctx, p, "/skip/deep", 0)
	simplefs.WriteFile(ctx, p, "/a/b/f", nil, 0)
	simplefs.WriteFile(ctx, p, "/a/g", nil, 0)
	simplefs.WriteFile(ctx, p, "/skip/deep/h", nil, 0)

	var seen []string
	err := simplefs.WalkDir(ctx, p, "/", func(path string, d simplefs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		seen = append(seen, path)
		if path == "/skip" {
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/", "/a", "/a/b", "/a/b/f", "/a/g", "/skip"}
	if !slices.Equal(seen, want) {
		t.Fatalf("walk = %v, want %v", seen, want)
	}

	var n int
	simplefs.WalkDir(ctx, p, "/", func(path string, d simplefs.DirEntry, err error) error {
		n++
		return fs.SkipAll
	})
	if n != 1 {
		t.Fatalf("SkipAll visited %d paths", n)
	}

	err = simplefs.WalkDir(ctx, p, "/missing", func(path string, d simplefs.DirEntry, err error) error { return err })
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("walk missing root = %v", err)
	}
}

func TestCopy(t *testing.T) {
	ctx := t.Context()
	src, dst := memfs.New(), memfs.New()
	data := make([]byte, 3<<20+17)
	for i := range data {
		data[i] = byte(i)
	}
	simplefs.WriteFile(ctx, src, "/big", data, 0o640)
	n, err := simplefs.Copy(ctx, dst, "/copy", src, "/big")
	if err != nil || n != int64(len(data)) {
		t.Fatalf("Copy = %d, %v", n, err)
	}
	got, _ := simplefs.ReadFile(ctx, dst, "/copy")
	if !slices.Equal(got, data) {
		t.Fatal("content differs")
	}
	if info, _ := dst.Stat(ctx, "/copy"); info.Mode != 0o640 {
		t.Fatalf("mode = %v", info.Mode)
	}
	if _, err := simplefs.Copy(ctx, dst, "/d", src, "/"); !errors.Is(err, simplefs.ErrIsDir) {
		t.Fatalf("Copy(dir) = %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := simplefs.Copy(cctx, dst, "/c2", src, "/big"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Copy(cancelled) = %v", err)
	}
}
