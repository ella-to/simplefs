//go:build linux || windows

package mount

// On Linux and Windows a mount is an in-process NFSv3 server (ella.to/
// simplefs/nfs — pure Go, no FUSE, no drivers) attached through the kernel's
// native NFS client:
//
//   - Linux:   syscall.Mount with string options; the kernel performs the
//     MOUNT handshake itself, so this works in a minimal container with
//     CAP_SYS_ADMIN and needs no nfs-utils.
//   - Windows: mount.exe from the built-in "Client for NFS" (Pro/Enterprise).
//
// Unlike macOS — where the provider lives inside a system-managed extension
// process — the provider here is the one registered in *this* process via
// Register, and Mount/Unmount/List track this process's mounts only.
//
// To serve without mounting (e.g. from an unprivileged Docker container,
// with the volume consumed outside), skip this package and run nfs.Server
// directly; see examples/nfsserver.

import (
	"errors"
	"fmt"
	"sync"

	"ella.to/simplefs/nfs"
)

// ErrUnsupported is kept for API compatibility with platforms that have no
// mount backend; on Linux and Windows it is only returned for misuse (no
// registered provider).
var ErrUnsupported = errors.New("mount: no provider registered (call mount.Register first)")

type nfsMount struct {
	domainID    string
	displayName string
	srv         *nfs.Server
	target      string // mountpoint (Linux) or drive letter (Windows)
}

var (
	nfsMu     sync.Mutex
	nfsMounts = map[string]*nfsMount{}
)

// Mount starts an in-process NFS server for the registered provider and
// attaches it with the operating system's native NFS client. It returns the
// path the filesystem is reachable at (a directory on Linux, a drive like
// "Z:\" on Windows).
func Mount(domainID, displayName string) (string, error) {
	p := Current()
	if p == nil {
		return "", ErrUnsupported
	}

	nfsMu.Lock()
	defer nfsMu.Unlock()
	if m, ok := nfsMounts[domainID]; ok {
		return m.target, nil
	}

	srv := nfs.NewServer(p, nfsOptions())
	if err := srv.Start(nfsListenAddr()); err != nil {
		return "", fmt.Errorf("mount: start nfs server: %w", err)
	}
	target, err := nfsAttach(domainID, srv.Port())
	if err != nil {
		srv.Close()
		return "", err
	}
	nfsMounts[domainID] = &nfsMount{
		domainID:    domainID,
		displayName: displayName,
		srv:         srv,
		target:      target,
	}
	return target, nil
}

// Unmount detaches the domain's filesystem and stops its NFS server.
func Unmount(domainID string) error {
	nfsMu.Lock()
	m, ok := nfsMounts[domainID]
	delete(nfsMounts, domainID)
	nfsMu.Unlock()
	if !ok {
		return fmt.Errorf("mount: domain %q is not mounted by this process", domainID)
	}
	err := nfsDetach(m.target)
	m.srv.Close()
	return err
}

// List returns the domains mounted by this process.
func List() ([]Domain, error) {
	nfsMu.Lock()
	defer nfsMu.Unlock()
	var out []Domain
	for _, m := range nfsMounts {
		out = append(out, Domain{ID: m.domainID, DisplayName: m.displayName})
	}
	return out, nil
}

// UnmountAll unmounts every domain mounted by this process.
func UnmountAll() error {
	domains, _ := List()
	var firstErr error
	for _, d := range domains {
		if err := Unmount(d.ID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
