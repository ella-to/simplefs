// Package mount attaches a simplefs.FileProvider to the operating system's
// file tree — with no kernel extensions, no FUSE, and no drivers.
//
// Mount starts an in-process, loopback-only NFSv3 server (package nfs) for
// the provider and attaches it with the client the OS already ships:
//
//   - Linux:   the mount(2) system call. The kernel performs the MOUNT
//     handshake itself, so this works in a minimal container; it needs
//     CAP_SYS_ADMIN (root, or docker run --cap-add SYS_ADMIN).
//   - macOS:   /sbin/mount_nfs. No app bundle, no signing.
//   - Windows: mount.exe from the "Client for NFS" feature (Pro, Enterprise,
//     Education, Server), mapped to a drive letter.
//   - FreeBSD: mount_nfs (needs root).
//
// The provider lives in the calling process: when the process exits the
// mount stops working, so always Unmount (or Close) it. CleanupStale detaches
// mounts left behind by a process that crashed.
//
// To serve without mounting — e.g. from an unprivileged Docker container
// whose volume is mounted from outside — use nfs.Server directly. For a
// Finder-integrated macOS mount, see package fileprovider.
package mount

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"ella.to/simplefs"
	"ella.to/simplefs/nfs"
)

var (
	// ErrUnsupported reports that no mount backend exists for this
	// operating system (or for the requested Backend).
	ErrUnsupported = errors.New("mount: not supported on this platform")

	// ErrNoPrivilege reports that the OS refused the mount for lack of
	// privileges; the error message says what is needed.
	ErrNoPrivilege = errors.New("mount: insufficient privileges")

	// ErrNoClient reports that the OS's NFS (or WebDAV) client is missing
	// or disabled; the error message says how to enable it.
	ErrNoClient = errors.New("mount: the operating system's network filesystem client is unavailable")
)

// Backend selects how the provider is attached.
type Backend int

const (
	// Auto picks the best backend for the platform: NFS everywhere, with
	// WebDAV as the fallback on Windows editions without Client for NFS.
	Auto Backend = iota

	// NFS serves NFSv3 on loopback and mounts it with the OS NFS client.
	NFS

	// WebDAV serves WebDAV on loopback and mounts it with the OS WebDAV
	// client (Windows WebClient).
	WebDAV
)

// String returns "auto", "nfs", or "webdav".
func (b Backend) String() string {
	switch b {
	case Auto:
		return "auto"
	case NFS:
		return "nfs"
	case WebDAV:
		return "webdav"
	default:
		return fmt.Sprintf("Backend(%d)", int(b))
	}
}

// Options configure Mount. The zero value mounts read-write at an
// automatically chosen path.
type Options struct {
	// Path is where the filesystem appears: a directory on Unix (created if
	// missing), or a drive such as "Z:" on Windows. Empty picks one: a new
	// directory under $XDG_RUNTIME_DIR or the temp dir (Linux, FreeBSD),
	// under ~/Library/Caches/simplefs (macOS), or a free drive letter
	// (Windows).
	Path string

	// Name labels the mount in logs and auto-chosen paths. Default
	// "simplefs".
	Name string

	// ReadOnly mounts read-only; the provider is also wrapped with
	// simplefs.ReadOnly so no mutation can reach it.
	ReadOnly bool

	// Backend selects the attach mechanism. Default Auto.
	Backend Backend

	// Logger receives operational messages. Nil discards them.
	Logger *slog.Logger

	// NFS overrides the server options. Mount fills UID/GID (the calling
	// user, when both are zero), Logger, and anything the platform needs.
	NFS nfs.Options
}

// Point is an active mount. Its methods are safe for concurrent use.
type Point struct {
	path   string
	name   string
	log    *slog.Logger
	server interface {
		Shutdown(context.Context) error
	}
	detach  func() error
	cleanup func() // removes an auto-created mount directory

	once sync.Once
	err  error
	done chan struct{}
}

// Path returns where the filesystem is reachable: a directory, or a drive
// such as `Z:\` on Windows.
func (m *Point) Path() string { return m.path }

// Done is closed once the mount has been unmounted.
func (m *Point) Done() <-chan struct{} { return m.done }

// Unmount detaches the filesystem and stops its server, waiting (up to ctx)
// for in-flight requests to finish. It is idempotent: later calls return the
// first call's result.
func (m *Point) Unmount(ctx context.Context) error {
	m.once.Do(func() {
		defer close(m.done)
		err := m.detach()
		if serr := m.server.Shutdown(ctx); err == nil && serr != nil && !errors.Is(serr, nfs.ErrServerClosed) {
			err = serr
		}
		if m.cleanup != nil {
			m.cleanup()
		}
		m.err = err
		m.log.Info("mount: unmounted", "name", m.name, "path", m.path, "err", err)
	})
	return m.err
}

// Close is Unmount with a background context, for use with defer.
func (m *Point) Close() error { return m.Unmount(context.Background()) }

// Mount attaches p to the file tree as described by opts and returns the
// active mount. Always Unmount it before the process exits.
func Mount(ctx context.Context, p simplefs.FileProvider, opts Options) (*Point, error) {
	if p == nil {
		return nil, errors.New("mount: nil provider")
	}
	if opts.Name == "" {
		opts.Name = "simplefs"
	}
	if err := simplefs.ValidName(opts.Name); err != nil {
		return nil, fmt.Errorf("mount: Options.Name: %w", err)
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.ReadOnly {
		p = simplefs.ReadOnly(p)
	}
	backend := opts.Backend
	if backend == Auto {
		backend = autoBackend()
	}
	switch backend {
	case NFS:
		return mountNFS(ctx, p, opts)
	case WebDAV:
		return mountWebDAV(ctx, p, opts)
	default:
		return nil, fmt.Errorf("%w: backend %v", ErrUnsupported, backend)
	}
}

// attachConfig is what a platform's attach function needs.
type attachConfig struct {
	port     int    // loopback port of the server
	path     string // requested mount path; empty = choose one
	name     string
	readOnly bool
}

func mountNFS(ctx context.Context, p simplefs.FileProvider, opts Options) (*Point, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := CleanupStale(ctx); err != nil {
		opts.Logger.Warn("mount: cleaning up stale mounts failed", "err", err)
	}

	nopts := opts.NFS
	if nopts.UID == 0 && nopts.GID == 0 {
		nopts.UID, nopts.GID = currentIDs()
	}
	if nopts.Logger == nil {
		nopts.Logger = opts.Logger
	}
	nopts.ReadOnly = nopts.ReadOnly || opts.ReadOnly
	if nopts.AttrCacheTTL == 0 {
		nopts.AttrCacheTTL = time.Second // matches the client's actimeo=1
	}
	addr := nfsPlatform(&nopts)

	srv := nfs.NewServer(p, nopts)
	if err := srv.Start(addr); err != nil {
		return nil, fmt.Errorf("mount: start NFS server: %w", err)
	}
	target, cleanup, err := attachNFS(ctx, attachConfig{
		port:     srv.Port(),
		path:     opts.Path,
		name:     opts.Name,
		readOnly: opts.ReadOnly,
	})
	if err != nil {
		srv.Close()
		return nil, err
	}
	opts.Logger.Info("mount: mounted", "name", opts.Name, "path", target, "backend", "nfs", "port", srv.Port())
	return &Point{
		path:    target,
		name:    opts.Name,
		log:     opts.Logger,
		server:  srv,
		detach:  func() error { return detachNFS(target) },
		cleanup: cleanup,
		done:    make(chan struct{}),
	}, nil
}

// currentIDs returns the calling user's uid and gid (0 where the OS has
// none, e.g. Windows).
func currentIDs() (uid, gid uint32) {
	return uint32(max(os.Getuid(), 0)), uint32(max(os.Getgid(), 0))
}

// autoDir creates a fresh mount directory under base for an auto-chosen
// path and returns it with a cleanup func that removes it again.
func autoDir(base, name string) (string, func(), error) {
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", nil, fmt.Errorf("mount: create %s: %w", base, err)
	}
	dir, err := os.MkdirTemp(base, name+"-")
	if err != nil {
		return "", nil, fmt.Errorf("mount: create mount point: %w", err)
	}
	return dir, func() { os.Remove(dir) }, nil
}

// ensureDir makes sure an explicitly requested Unix mount path exists and
// returns a cleanup func that removes it again only if Mount created it.
func ensureDir(path string) (func(), error) {
	fi, err := os.Stat(path)
	switch {
	case err == nil && fi.IsDir():
		return func() {}, nil
	case err == nil:
		return nil, fmt.Errorf("mount: %s is not a directory", path)
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("mount: %w", err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("mount: create %s: %w", path, err)
	}
	return func() { os.Remove(path) }, nil
}
