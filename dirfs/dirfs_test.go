package dirfs_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"ella.to/simplefs"
	"ella.to/simplefs/dirfs"
)

func TestCannotEscape(t *testing.T) {
	outer := t.TempDir()
	inner := filepath.Join(outer, "inner")
	os.Mkdir(inner, 0o755)
	os.WriteFile(filepath.Join(outer, "secret"), []byte("s"), 0o644)
	if runtime.GOOS != "windows" {
		os.Symlink("../secret", filepath.Join(inner, "escape"))
	}

	p, err := dirfs.New(inner)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx := t.Context()

	if _, err := p.Stat(ctx, "/../secret"); err == nil {
		t.Fatal(`Stat("/../secret") reached outside the root`)
	}
	if runtime.GOOS != "windows" {
		if _, err := simplefs.ReadFile(ctx, p, "/escape"); err == nil {
			t.Fatal("following a symlink reached outside the root")
		}
	}
}

func TestWriteThroughToDisk(t *testing.T) {
	dir := t.TempDir()
	p, err := dirfs.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := simplefs.WriteFile(t.Context(), p, "/hello.txt", []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil || string(data) != "hi" {
		t.Fatalf("on disk: %q, %v", data, err)
	}
}
