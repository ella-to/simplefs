// Command dirfs exposes a directory of this machine through simplefs: the
// dirfs provider (all access confined with os.Root), optionally narrowed to a
// subfolder with simplefs.Sub and made read-only with simplefs.ReadOnly, then
// served over WebDAV or NFS, or mounted.
//
//	go run ./examples/dirfs --dir ~/Documents --sub Projects --readonly --mount ~/mnt/projects
//	go run ./examples/dirfs --dir ./data --webdav 127.0.0.1:8080   # open in a browser or Finder
//	go run ./examples/dirfs --dir ./data --nfs 127.0.0.1:2049
//
// Without --mount/--webdav/--nfs it prints the top of the tree and exits.
package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"

	"ella.to/simplefs"
	"ella.to/simplefs/dirfs"
	"ella.to/simplefs/mount"
	"ella.to/simplefs/nfs"
	"ella.to/simplefs/webdav"
)

func main() {
	dir := flag.String("dir", ".", "directory to expose")
	sub := flag.String("sub", "", "expose only this subfolder of --dir (it becomes the root)")
	readOnly := flag.Bool("readonly", false, "refuse every write")
	mountPath := flag.String("mount", "", "mount at this path")
	davAddr := flag.String("webdav", "", "serve WebDAV on this loopback address")
	nfsAddr := flag.String("nfs", "", "serve NFSv3 on this loopback address")
	flag.Parse()
	if err := run(*dir, *sub, *readOnly, *mountPath, *davAddr, *nfsAddr); err != nil {
		fmt.Fprintln(os.Stderr, "dirfs:", err)
		os.Exit(1)
	}
}

func run(dir, sub string, readOnly bool, mountPath, davAddr, nfsAddr string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	d, err := dirfs.New(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	var p simplefs.FileProvider = d
	if sub != "" {
		// Sub jails the view: "/" is the subfolder and ".." cannot leave it.
		p = simplefs.Sub(p, sub)
	}
	if readOnly {
		p = simplefs.ReadOnly(p)
	}
	if _, err := p.Stat(ctx, "/"); err != nil {
		return err
	}

	switch {
	case mountPath != "":
		m, err := mount.Mount(ctx, p, mount.Options{Path: mountPath, Name: "dirfs", ReadOnly: readOnly})
		if err != nil {
			return err
		}
		fmt.Printf("mounted %s at %s — Ctrl-C to unmount\n", dir, m.Path())
		<-ctx.Done()
		return m.Close()

	case davAddr != "":
		ln, err := net.Listen("tcp", davAddr)
		if err != nil {
			return err
		}
		srv := &http.Server{Handler: webdav.NewHandler(p, webdav.Options{ReadOnly: readOnly})}
		go srv.Serve(ln)
		fmt.Printf("WebDAV on http://%s/ — Finder: Go > Connect to Server; Windows: net use Z: http://%s/\n", ln.Addr(), ln.Addr())
		<-ctx.Done()
		return srv.Shutdown(context.Background())

	case nfsAddr != "":
		srv := nfs.NewServer(p, nfs.Options{ReadOnly: readOnly})
		if err := srv.Start(nfsAddr); err != nil {
			return err
		}
		fmt.Printf("NFSv3 on %s (port %d) — Ctrl-C to stop\n", nfsAddr, srv.Port())
		<-ctx.Done()
		return srv.Shutdown(context.Background())
	}

	// No server: just show what the provider sees, through io/fs.
	return fs.WalkDir(simplefs.FS(ctx, p), ".", func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fmt.Println(path)
		if e.IsDir() && path != "." {
			return fs.SkipDir // one level is enough for a demo
		}
		return nil
	})
}
