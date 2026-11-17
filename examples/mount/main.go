// Command mount attaches an in-memory provider to the file tree on any OS
// with package mount, and detaches it cleanly on Ctrl-C.
//
//	go run ./examples/mount                        # auto-chosen path
//	go run ./examples/mount --path ~/mnt/demo      # explicit path (Windows: --path Z:)
//	go run ./examples/mount --backend webdav       # macOS, Windows (any edition)
//	go run ./examples/mount --readonly
//	go run ./examples/mount --cleanup              # only detach stale mounts and exit
//
// Linux needs CAP_SYS_ADMIN (sudo, or docker run --cap-add SYS_ADMIN);
// macOS needs nothing; Windows needs "Client for NFS" (NFS) or the WebClient
// service (WebDAV). The errors say what is missing.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"ella.to/simplefs"
	"ella.to/simplefs/memfs"
	"ella.to/simplefs/mount"
)

func main() {
	path := flag.String("path", "", "where to mount (empty: choose one)")
	backend := flag.String("backend", "auto", "auto, nfs or webdav")
	readOnly := flag.Bool("readonly", false, "mount read-only")
	cleanup := flag.Bool("cleanup", false, "detach mounts left by crashed processes, then exit")
	flag.Parse()
	if err := run(*path, *backend, *readOnly, *cleanup); err != nil {
		fmt.Fprintln(os.Stderr, "mount:", err)
		os.Exit(1)
	}
}

func run(path, backend string, readOnly, cleanup bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cleanup {
		return mount.CleanupStale(ctx)
	}
	b, err := parseBackend(backend)
	if err != nil {
		return err
	}

	p := memfs.New()
	simplefs.WriteFile(ctx, p, "/README.txt", []byte("This folder is served by a Go process.\n"), 0)

	// Bound how long attaching may take; the mount itself lives on.
	mctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	m, err := mount.Mount(mctx, p, mount.Options{Path: path, Name: "demo", ReadOnly: readOnly, Backend: b})
	switch {
	case errors.Is(err, mount.ErrNoPrivilege):
		return fmt.Errorf("%w\n(hint: on Linux run with sudo, or serve with examples/nfsserver and mount from outside)", err)
	case errors.Is(err, mount.ErrNoClient):
		return fmt.Errorf("%w\n(hint: install or enable the OS client named above, or try --backend webdav)", err)
	case errors.Is(err, mount.ErrUnsupported):
		return fmt.Errorf("%w\n(hint: serve with examples/nfsserver and mount from a supported OS)", err)
	case err != nil:
		return err
	}
	// Always detach, even on a panic further down.
	defer m.Close()

	fmt.Printf("mounted at %s — try:\n  ls %s\n  echo hi > %s\nCtrl-C to unmount\n",
		m.Path(), m.Path(), filepath.Join(m.Path(), "hello.txt"))

	// Everything written through the mount lands in the provider.
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			uctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return m.Unmount(uctx)
		case <-m.Done():
			return nil
		case <-t.C:
			n := 0
			for _, err := range simplefs.Entries(ctx, p, "/") {
				if err == nil {
					n++
				}
			}
			fmt.Printf("provider holds %d entries at the root\n", n)
		}
	}
}

func parseBackend(s string) (mount.Backend, error) {
	switch s {
	case "auto":
		return mount.Auto, nil
	case "nfs":
		return mount.NFS, nil
	case "webdav":
		return mount.WebDAV, nil
	}
	return 0, fmt.Errorf("unknown --backend %q", s)
}
