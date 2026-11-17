// Command sqlitefs demonstrates a simplefs provider stored in a single
// SQLite file (package sqlitefs, pure Go: no cgo, any OS).
//
//	cd examples/sqlitefs
//	go run .                           # uses ./data.db
//	go run . --db /tmp/x.db            # run it twice: the run counter persists
//	go run . --serve 127.0.0.1:2049    # then serve the database over NFSv3
//	go run . --mount ~/mnt/sqlitefs    # or mount it (see package mount)
//
// Each run opens the database, bumps a counter kept in /runs.txt, writes a
// few files through the simplefs helpers (WriteFile, MkdirAll, a symlink),
// then reads everything back through plain io/fs — fs.WalkDir and
// fs.ReadFile on simplefs.FS — to show that code written against the
// standard library works unchanged on a database-backed tree. With --serve
// or --mount the same tree is then exported until Ctrl-C; whatever a client
// writes is in the database next time.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ella.to/simplefs"
	"ella.to/simplefs/examples/sqlitefs/sqlitefs"
	"ella.to/simplefs/mount"
	"ella.to/simplefs/nfs"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sqlitefs:", err)
		os.Exit(1)
	}
}

func run() error {
	dbPath := flag.String("db", "data.db", "SQLite database file (created if missing)")
	serve := flag.String("serve", "", "after the demo, serve the database over NFSv3 on this address (e.g. 127.0.0.1:2049)")
	mountPath := flag.String("mount", "", "after the demo, mount the database at this path")
	flag.Parse()
	if *serve != "" && *mountPath != "" {
		return errors.New("use --serve or --mount, not both")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	p, err := sqlitefs.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer p.Close()

	runs, err := bumpCounter(ctx, p)
	if err != nil {
		return err
	}
	fmt.Printf("database %s — run #%d\n", *dbPath, runs)

	if err := writeTree(ctx, p, runs); err != nil {
		return err
	}
	if err := printTree(simplefs.FS(ctx, p)); err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	switch {
	case *serve != "":
		return serveNFS(ctx, p, *serve, log)
	case *mountPath != "":
		return mountAt(ctx, p, *mountPath, log)
	}
	return nil
}

// bumpCounter increments the number stored in /runs.txt (0 when missing)
// and returns the new value. That it survives restarts is the point.
func bumpCounter(ctx context.Context, p simplefs.FileProvider) (int, error) {
	n := 0
	data, err := simplefs.ReadFile(ctx, p, "/runs.txt")
	switch {
	case err == nil:
		if n, err = strconv.Atoi(strings.TrimSpace(string(data))); err != nil {
			return 0, fmt.Errorf("/runs.txt: %w", err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return 0, err
	}
	n++
	return n, simplefs.WriteFile(ctx, p, "/runs.txt", []byte(strconv.Itoa(n)+"\n"), 0)
}

// writeTree adds this run's files using the provider-level helpers.
func writeTree(ctx context.Context, p simplefs.FileProvider, run int) error {
	for _, dir := range []string{"/docs/runs", "/data"} {
		if err := simplefs.MkdirAll(ctx, p, dir, 0); err != nil {
			return err
		}
	}
	readme := "This tree lives in one SQLite file: a nodes table (the tree) and a\n" +
		"chunks table (file content in 64 KiB blocks).\n"
	if err := simplefs.WriteFile(ctx, p, "/docs/README.txt", []byte(readme), 0); err != nil {
		return err
	}
	note := fmt.Sprintf("run %d at %s\n", run, time.Now().Format(time.RFC3339))
	name := fmt.Sprintf("/docs/runs/%03d.txt", run)
	if err := simplefs.WriteFile(ctx, p, name, []byte(note), 0); err != nil {
		return err
	}

	// A file spanning several chunks, rewritten every run.
	big := make([]byte, 3*sqlitefs.ChunkSize+1000)
	for i := range big {
		big[i] = byte('a' + i%26)
	}
	if err := simplefs.WriteFile(ctx, p, "/data/alphabet.bin", big, 0); err != nil {
		return err
	}

	// Point /latest at this run's note (symlinks are an optional capability).
	if sp, ok := simplefs.As[simplefs.SymlinkProvider](p); ok {
		if err := p.Remove(ctx, "/latest", simplefs.RemoveOptions{Force: true}); err != nil {
			return err
		}
		if err := sp.Symlink(ctx, strings.TrimPrefix(name, "/"), "/latest"); err != nil {
			return err
		}
	}
	return nil
}

// printTree is ordinary io/fs code: it knows nothing about simplefs or SQLite.
func printTree(fsys fs.FS) error {
	fmt.Println("tree (via io/fs):")
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		indent := strings.Repeat("  ", strings.Count(name, "/")+1)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			fmt.Printf("%s%s/\n", indent, d.Name())
		case d.Type()&fs.ModeSymlink != 0:
			target := "(symlink)"
			if rl, ok := fsys.(fs.ReadLinkFS); ok {
				target, _ = rl.ReadLink(name)
			}
			fmt.Printf("%s%s -> %s\n", indent, d.Name(), target)
		default:
			fmt.Printf("%s%s  %d bytes  %s\n", indent, d.Name(), info.Size(), info.Mode())
		}
		return nil
	})
	if err != nil {
		return err
	}

	// fs.ReadFile follows the symlink, as io/fs promises.
	latest, err := fs.ReadFile(fsys, "latest")
	if err != nil {
		return err
	}
	fmt.Printf("latest: %s", latest)
	big, err := fs.ReadFile(fsys, "data/alphabet.bin")
	if err != nil {
		return err
	}
	fmt.Printf("data/alphabet.bin: %d bytes, starts %q\n", len(big), big[:26])
	return nil
}

func serveNFS(ctx context.Context, p simplefs.FileProvider, addr string, log *slog.Logger) error {
	srv := nfs.NewServer(p, nfs.Options{
		UID:    uint32(max(os.Getuid(), 0)),
		GID:    uint32(max(os.Getgid(), 0)),
		Logger: log,
	})
	if err := srv.Start(addr); err != nil {
		return err
	}
	port := srv.Port()
	fmt.Printf("serving NFSv3 on %s (port %d) — Ctrl-C to stop. Mount it:\n", addr, port)
	fmt.Printf("  Linux: sudo mount -t nfs -o vers=3,proto=tcp,mountproto=tcp,port=%d,mountport=%d,nolock <host>:/ /mnt/sqlitefs\n", port, port)
	fmt.Printf("  macOS: mount_nfs -o vers=3,tcp,port=%d,mountport=%d,noresvport,locallocks <host>:/ ~/mnt/sqlitefs\n", port, port)
	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func mountAt(ctx context.Context, p simplefs.FileProvider, path string, log *slog.Logger) error {
	m, err := mount.Mount(ctx, p, mount.Options{Path: path, Name: "sqlitefs", Logger: log})
	if err != nil {
		return err
	}
	fmt.Printf("mounted at %s — Ctrl-C to unmount\n", m.Path())
	<-ctx.Done()

	unmountCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return m.Unmount(unmountCtx)
}
