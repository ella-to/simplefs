// Command nfsserver serves a provider over NFSv3 (pure Go, no FUSE, no
// drivers), or serves and mounts it in one step.
//
// Serve (any platform, no privileges — the Docker story):
//
//	nfsserver                               # memfs on 127.0.0.1:2049
//	nfsserver --backend dir --dir ./data    # a host directory instead
//	nfsserver --addr :2049 --allow 10.0.0.0/8,192.168.0.0/16
//	nfsserver --readonly                    # refuse every write
//	nfsserver --portmap                     # also answer PORTMAP on :111 (Windows clients)
//
// The server only accepts loopback clients unless --allow says otherwise; it
// has no authentication, so allow only networks you trust.
//
// Mount (Linux with CAP_SYS_ADMIN, macOS, Windows with Client for NFS):
//
//	nfsserver --mount /mnt/simplefs
//
// SIGINT/SIGTERM shut down gracefully: the mount is detached and in-flight
// requests finish. To serve your own backend, add it to openProvider.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"ella.to/simplefs"
	"ella.to/simplefs/dirfs"
	"ella.to/simplefs/memfs"
	"ella.to/simplefs/mount"
	"ella.to/simplefs/nfs"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "nfsserver:", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", "127.0.0.1:2049", "listen address (serve mode)")
	backend := flag.String("backend", "mem", "provider: mem (in memory) or dir (a host directory)")
	dir := flag.String("dir", ".", "directory to serve with --backend dir")
	allow := flag.String("allow", "", "comma-separated client networks (CIDR); default loopback only")
	readOnly := flag.Bool("readonly", false, "reject every write")
	portmap := flag.Bool("portmap", false, "also answer PORTMAP on port 111 (needed by Windows clients)")
	uid := flag.Uint("uid", uint(max(os.Getuid(), 0)), "uid reported as the owner of every file")
	gid := flag.Uint("gid", uint(max(os.Getgid(), 0)), "gid reported as the owner of every file")
	mountPath := flag.String("mount", "", "serve and mount at this path instead (see package mount)")
	verbose := flag.Bool("v", false, "log every RPC")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	provider, closeProvider, err := openProvider(*backend, *dir)
	if err != nil {
		return err
	}
	defer closeProvider()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *mountPath != "" {
		m, err := mount.Mount(ctx, provider, mount.Options{Path: *mountPath, ReadOnly: *readOnly, Logger: log})
		if err != nil {
			return err
		}
		fmt.Printf("mounted at %s — Ctrl-C to unmount\n", m.Path())
		<-ctx.Done()
		return unmount(m)
	}

	clients, err := parsePrefixes(*allow)
	if err != nil {
		return err
	}
	srv := nfs.NewServer(provider, nfs.Options{
		UID:            uint32(*uid),
		GID:            uint32(*gid),
		Portmap:        *portmap,
		ReadOnly:       *readOnly,
		AllowedClients: clients,
		Logger:         log,
	})
	if err := srv.Start(*addr); err != nil {
		return err
	}
	printMountHelp(srv.Port())
	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func unmount(m *mount.Point) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return m.Unmount(ctx)
}

// openProvider builds the provider selected by --backend.
func openProvider(backend, dir string) (simplefs.FileProvider, func(), error) {
	switch backend {
	case "mem":
		return memfs.New(), func() {}, nil
	case "dir":
		p, err := dirfs.New(dir)
		if err != nil {
			return nil, nil, err
		}
		return p, func() { p.Close() }, nil
	default:
		return nil, nil, fmt.Errorf("unknown --backend %q (want mem or dir)", backend)
	}
}

func parsePrefixes(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for f := range strings.SplitSeq(s, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			return nil, fmt.Errorf("--allow: %w", err)
		}
		out = append(out, p)
	}
	if len(out) == 0 && s != "" {
		return nil, errors.New("--allow: no networks given")
	}
	return out, nil
}

func printMountHelp(port int) {
	fmt.Printf("serving NFSv3 on port %d — Ctrl-C to stop. Mount it:\n", port)
	fmt.Printf("  Linux:   sudo mount -t nfs -o vers=3,proto=tcp,mountproto=tcp,port=%d,mountport=%d,nolock <host>:/ /mnt/simplefs\n", port, port)
	fmt.Printf("  macOS:   mount_nfs -o vers=3,tcp,port=%d,mountport=%d,noresvport,locallocks <host>:/ ~/mnt/simplefs\n", port, port)
	fmt.Printf("  Windows: mount -o anon,nolock \\\\<host>\\simplefs Z:   (needs --portmap and port 2049)\n")
	fmt.Printf("  Docker:  docker volume create -d local -o type=nfs -o o=addr=<host>,port=%d,mountport=%d,vers=3,tcp,nolock -o device=:/ simplefs\n", port, port)
}
