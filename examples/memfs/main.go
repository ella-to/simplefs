// Command memfs demonstrates the simplefs.FileProvider interface end-to-end against
// the in-memory implementation: it creates directories and files, writes and
// reads content, renames, deletes, and shows the optional capabilities
// (symlinks, xattrs, statfs) plus a live watcher reporting every change.
//
// Everything is stored in process memory — run it from anywhere:
//
//	go run ./examples/memfs
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"time"

	"ella.to/simplefs"
	"ella.to/simplefs/memfs"
)

func main() {
	ctx := context.Background()

	// The example is written entirely against the interfaces, not *memfs.MemFS:
	// swap in any other simplefs.FileProvider and everything below still works.
	var provider simplefs.FileProvider = memfs.New()

	// Watch the whole tree so every operation below reports itself.
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	events, err := provider.(simplefs.WatchProvider).Watch(watchCtx, "/", true)
	check(err)
	go func() {
		for ev := range events {
			if ev.Type == simplefs.EventRename {
				fmt.Printf("  [event] %-6s %s -> %s\n", ev.Type, ev.OldPath, ev.Path)
				continue
			}
			fmt.Printf("  [event] %-6s %s\n", ev.Type, ev.Path)
		}
	}()

	fmt.Println("== create a directory and a file ==")
	check(provider.Mkdir(ctx, "/docs", simplefs.MkdirOptions{}))

	h, err := provider.Create(ctx, "/docs/hello.txt", simplefs.CreateOptions{Mode: 0o644})
	check(err)
	_, err = h.WriteAt([]byte("hello from simplefs\n"), 0)
	check(err)
	check(h.Sync(ctx))
	check(h.Close())

	fmt.Println("\n== read it back ==")
	fmt.Printf("  /docs/hello.txt: %q\n", string(readFile(ctx, provider, "/docs/hello.txt")))

	fmt.Println("\n== update it (append via random access) ==")
	h, err = provider.Open(ctx, "/docs/hello.txt", simplefs.OpenOptions{Read: true, Write: true})
	check(err)
	info, err := h.Stat(ctx)
	check(err)
	_, err = h.WriteAt([]byte("updated in memory\n"), info.Size)
	check(err)
	check(h.Close())
	fmt.Printf("  /docs/hello.txt: %q\n", string(readFile(ctx, provider, "/docs/hello.txt")))

	fmt.Println("\n== stat ==")
	info, err = provider.Stat(ctx, "/docs/hello.txt")
	check(err)
	fmt.Printf("  path=%s type=%s size=%d mode=%o etag=%s\n",
		info.Path, info.Type, info.Size, info.Mode, info.ETag)

	fmt.Println("\n== list a directory with pagination (2 entries per page) ==")
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		writeFile(ctx, provider, "/docs/"+name, []byte(name))
	}
	page := simplefs.Page{Limit: 2}
	for pageNo := 1; ; pageNo++ {
		dir, err := provider.ReadDir(ctx, "/docs", page)
		check(err)
		for _, e := range dir.Entries {
			fmt.Printf("  page %d: %-10s (%s, %d bytes)\n", pageNo, e.Name, e.Type, e.Info.Size)
		}
		if dir.NextToken == "" {
			break
		}
		page.Token = dir.NextToken
	}

	fmt.Println("\n== rename and move ==")
	check(provider.Mkdir(ctx, "/archive", simplefs.MkdirOptions{}))
	check(provider.Rename(ctx, "/docs/hello.txt", "/archive/hello-2026.txt", simplefs.RenameOptions{}))
	fmt.Printf("  /archive/hello-2026.txt: %q\n", string(readFile(ctx, provider, "/archive/hello-2026.txt")))

	fmt.Println("\n== optional capabilities ==")
	check(provider.(simplefs.SymlinkProvider).Symlink(ctx, "/archive/hello-2026.txt", "/docs/latest"))
	target, err := provider.(simplefs.SymlinkProvider).Readlink(ctx, "/docs/latest")
	check(err)
	fmt.Printf("  symlink /docs/latest -> %s\n", target)

	check(provider.(simplefs.XattrProvider).SetXattr(ctx, "/archive/hello-2026.txt", "user.author", []byte("ali")))
	names, err := provider.(simplefs.XattrProvider).ListXattr(ctx, "/archive/hello-2026.txt")
	check(err)
	fmt.Printf("  xattrs: %v\n", names)

	fsinfo, err := provider.(simplefs.StatFSProvider).StatFS(ctx)
	check(err)
	fmt.Printf("  statfs: %d/%d blocks free (block size %d)\n", fsinfo.Free, fsinfo.Blocks, fsinfo.BlockSize)

	fmt.Println("\n== delete ==")
	check(provider.Remove(ctx, "/docs", simplefs.RemoveOptions{Recursive: true}))
	_, err = provider.Stat(ctx, "/docs")
	fmt.Printf("  stat /docs after remove: %v\n", err)

	// Give the watcher goroutine a moment to print the trailing events.
	time.Sleep(50 * time.Millisecond)
	fmt.Println("\ndone — all state lived and died in memory.")
}

func readFile(ctx context.Context, p simplefs.FileProvider, path string) []byte {
	h, err := p.Open(ctx, path, simplefs.OpenOptions{Read: true})
	check(err)
	defer h.Close()
	info, err := h.Stat(ctx)
	check(err)
	data := make([]byte, info.Size)
	if _, err := h.ReadAt(data, 0); err != nil && err != io.EOF {
		check(err)
	}
	return data
}

func writeFile(ctx context.Context, p simplefs.FileProvider, path string, data []byte) {
	h, err := p.Create(ctx, path, simplefs.CreateOptions{Mode: 0o644})
	check(err)
	defer h.Close()
	_, err = h.WriteAt(data, 0)
	check(err)
	check(h.Sync(ctx))
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
