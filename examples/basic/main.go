// Command basic is a tour of the simplefs API on the in-memory provider: no
// mount, no privileges, any OS.
//
//	go run ./examples/basic
//
// It covers the helpers (WriteFile, ReadFile, MkdirAll, Entries, WalkDir,
// Copy, RemoveAll), raw handles (append, random access, truncate), paged
// ReadDir, renames with and without overwrite, every sentinel error matched
// with errors.Is (including io/fs ones), and optional capabilities found with
// simplefs.As. It exits non-zero if anything behaves unexpectedly.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"ella.to/simplefs"
	"ella.to/simplefs/memfs"
)

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "basic:", err)
		os.Exit(1)
	}
	fmt.Println("\nall checks passed")
}

func run(ctx context.Context) error {
	var p simplefs.FileProvider = memfs.New()

	section("helpers")
	must(simplefs.MkdirAll(ctx, p, "/docs/2026/notes", 0o755))
	must(simplefs.WriteFile(ctx, p, "/docs/hello.txt", []byte("hello, simplefs\n"), 0o644))
	data, err := simplefs.ReadFile(ctx, p, "/docs/hello.txt")
	must(err)
	fmt.Printf("ReadFile: %q\n", data)
	ok, err := simplefs.Exists(ctx, p, "/docs/2026/notes")
	must(err)
	fmt.Println("Exists(/docs/2026/notes):", ok)

	section("handles: append, random access, truncate")
	h, err := p.Open(ctx, "/log.txt", simplefs.OpenOptions{Write: true, Create: true, Append: true, Mode: 0o600})
	must(err)
	for i := range 3 {
		_, err := h.WriteAt([]byte(fmt.Sprintf("line %d\n", i)), 0) // offset ignored with Append
		must(err)
	}
	must(h.Close())
	h, err = p.Open(ctx, "/log.txt", simplefs.OpenOptions{Read: true, Write: true})
	must(err)
	if _, err := h.WriteAt([]byte("LINE"), 7); err != nil { // overwrite in the middle
		return err
	}
	must(h.Truncate(ctx, 14))
	buf := make([]byte, 64)
	n, err := h.ReadAt(buf, 0)
	if err != nil && err != io.EOF { // a short read reports io.EOF
		return err
	}
	info, _ := h.Stat(ctx)
	fmt.Printf("content %q, size %d, mode %v\n", buf[:n], info.Size, info.Mode)
	must(h.Close())
	expect(h.Close(), simplefs.ErrClosed, "second Close")

	section("paged ReadDir and Entries")
	for i := range 7 {
		must(simplefs.WriteFile(ctx, p, fmt.Sprintf("/docs/2026/notes/n%d.md", i), nil, 0))
	}
	page, err := p.ReadDir(ctx, "/docs/2026/notes", simplefs.Page{Limit: 3})
	must(err)
	fmt.Printf("first page: %d entries, next token %q\n", len(page.Entries), page.NextToken)
	var names []string
	for e, err := range simplefs.Entries(ctx, p, "/docs/2026/notes") { // drains every page
		must(err)
		names = append(names, e.Name)
	}
	fmt.Println("all entries:", strings.Join(names, " "))

	section("WalkDir")
	must(simplefs.WalkDir(ctx, p, "/", func(path string, d simplefs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == "/docs/2026/notes" {
			fmt.Println(path, "(skipped)")
			return fs.SkipDir
		}
		fmt.Printf("%-20s %s\n", path, d.Type)
		return nil
	}))

	section("rename")
	must(simplefs.WriteFile(ctx, p, "/a.txt", []byte("A"), 0))
	must(simplefs.WriteFile(ctx, p, "/b.txt", []byte("B"), 0))
	expect(p.Rename(ctx, "/a.txt", "/b.txt", simplefs.RenameOptions{}), simplefs.ErrExist, "rename onto existing")
	must(p.Rename(ctx, "/a.txt", "/b.txt", simplefs.RenameOptions{Overwrite: true}))
	data, _ = simplefs.ReadFile(ctx, p, "/b.txt")
	fmt.Printf("after overwrite: /b.txt = %q\n", data)
	must(p.Rename(ctx, "/docs", "/archive", simplefs.RenameOptions{})) // whole subtree moves
	expect(p.Rename(ctx, "/archive", "/archive/inside", simplefs.RenameOptions{}), simplefs.ErrInvalid, "move into itself")

	section("copy and remove")
	n64, err := simplefs.Copy(ctx, p, "/archive/copy.txt", p, "/archive/hello.txt")
	must(err)
	fmt.Println("copied bytes:", n64)
	expect(p.Remove(ctx, "/archive", simplefs.RemoveOptions{}), simplefs.ErrNotEmpty, "remove non-empty dir")
	must(simplefs.RemoveAll(ctx, p, "/archive"))
	must(p.Remove(ctx, "/archive", simplefs.RemoveOptions{Force: true})) // missing is fine with Force

	section("errors are io/fs errors")
	_, err = p.Stat(ctx, "/nope")
	expect(err, fs.ErrNotExist, "Stat missing (as fs.ErrNotExist)")
	var pe *fs.PathError
	if errors.As(err, &pe) {
		fmt.Printf("PathError: op=%s path=%s err=%v\n", pe.Op, pe.Path, pe.Err)
	}
	_, err = p.Open(ctx, "/", simplefs.OpenOptions{})
	expect(err, simplefs.ErrIsDir, "Open a directory")
	_, err = p.ReadDir(ctx, "/b.txt", simplefs.Page{})
	expect(err, simplefs.ErrNotDir, "ReadDir a file")
	_, err = p.Open(ctx, "/b.txt", simplefs.OpenOptions{Write: true, Create: true, Exclusive: true})
	expect(err, simplefs.ErrExist, "exclusive create")
	_, err = p.Open(ctx, "/b.txt", simplefs.OpenOptions{Read: true, Truncate: true})
	expect(err, simplefs.ErrInvalid, "Truncate without Write")
	expect(p.Mkdir(ctx, "/"+strings.Repeat("x", 300), simplefs.MkdirOptions{}), simplefs.ErrNameTooLong, "long name")
	small := memfs.New(memfs.WithCapacity(4))
	expect(simplefs.WriteFile(ctx, small, "/big", []byte("too big"), 0), simplefs.ErrNoSpace, "capacity")
	ro := simplefs.ReadOnly(p)
	expect(ro.Mkdir(ctx, "/x", simplefs.MkdirOptions{}), simplefs.ErrReadOnly, "read-only view")
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = p.Stat(cctx, "/")
	expect(err, context.Canceled, "cancelled context")

	section("capabilities via simplefs.As")
	if sp, ok := simplefs.As[simplefs.SymlinkProvider](p); ok {
		must(sp.Symlink(ctx, "/b.txt", "/link"))
		target, _ := sp.Readlink(ctx, "/link")
		fmt.Println("symlink /link ->", target)
	}
	if hp, ok := simplefs.As[simplefs.HardLinkProvider](p); ok {
		must(hp.Link(ctx, "/b.txt", "/b-hardlink.txt"))
		i1, _ := p.Stat(ctx, "/b.txt")
		i2, _ := p.Stat(ctx, "/b-hardlink.txt")
		fmt.Printf("hard link: same FileID %v, links %d\n", i1.FileID == i2.FileID, i1.Links)
	}
	if cp, ok := simplefs.As[simplefs.ChtimesProvider](p); ok {
		when := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		must(cp.Chtimes(ctx, "/b.txt", time.Time{}, when)) // zero atime: unchanged
		i, _ := p.Stat(ctx, "/b.txt")
		fmt.Println("mtime set to", i.ModTime.Format(time.DateOnly))
	}
	if xp, ok := simplefs.As[simplefs.XattrProvider](p); ok {
		must(xp.SetXattr(ctx, "/b.txt", "user.tag", []byte("blue")))
		names, _ := xp.ListXattr(ctx, "/b.txt")
		fmt.Println("xattrs:", names)
	}
	if sp, ok := simplefs.As[simplefs.StatFSProvider](p); ok {
		fsinfo, _ := sp.StatFS(ctx)
		fmt.Printf("statfs: %d of %d blocks free\n", fsinfo.Free, fsinfo.Blocks)
	}
	if wp, ok := simplefs.As[simplefs.WatchProvider](p); ok {
		wctx, stop := context.WithCancel(ctx)
		events, err := wp.Watch(wctx, "/", true)
		must(err)
		must(simplefs.WriteFile(ctx, p, "/watched.txt", []byte("x"), 0))
		for range 2 { // create + modify
			ev := <-events
			fmt.Printf("event: %s %s\n", ev.Type, ev.Path)
		}
		stop()
	}
	return nil
}

func section(name string) { fmt.Printf("\n== %s\n", name) }

// must panics on unexpected errors; this is a demo, not a library.
func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "basic: unexpected error:", err)
		os.Exit(1)
	}
}

// expect checks that err matches target with errors.Is.
func expect(err, target error, what string) {
	if !errors.Is(err, target) {
		fmt.Fprintf(os.Stderr, "basic: %s: got %v, want %v\n", what, err, target)
		os.Exit(1)
	}
	fmt.Printf("%-34s -> %v\n", what, target)
}
