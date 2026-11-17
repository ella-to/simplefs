package mount

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"ella.to/simplefs/nfs"
)

// nfsOptions reports files as owned by the mounting user, so the mount is
// usable without root once attached.
func nfsOptions() nfs.Options {
	return nfs.Options{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
}

// nfsListenAddr keeps the server loopback-only on an ephemeral port; the
// kernel client is pointed at it explicitly, so no portmapper is involved.
func nfsListenAddr() string { return "127.0.0.1:0" }

// nfsAttach mounts the loopback NFS server at a per-domain directory using
// the raw mount(2) syscall. With string options (vers=3 + port/mountport)
// the kernel performs the MOUNT-protocol handshake itself, so no userspace
// NFS tooling needs to exist — important inside minimal containers.
//
// This requires CAP_SYS_ADMIN (root, or a container run with
// --cap-add SYS_ADMIN / --privileged). Without it, serve instead of
// mounting: run nfs.Server directly and mount from outside the container.
func nfsAttach(domainID string, port int) (string, error) {
	target := filepath.Join(mountRoot(), domainID)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return "", fmt.Errorf("mount: create mountpoint: %w", err)
	}
	opts := fmt.Sprintf(
		"vers=3,proto=tcp,mountproto=tcp,port=%d,mountport=%d,addr=127.0.0.1,nolock,actimeo=1",
		port, port)
	if err := syscall.Mount("127.0.0.1:/", target, "nfs", 0, opts); err != nil {
		if errors.Is(err, syscall.EPERM) {
			return "", fmt.Errorf("mount: mounting needs CAP_SYS_ADMIN "+
				"(run as root, or docker run --cap-add SYS_ADMIN); to use an "+
				"unprivileged container, serve with nfs.Server and mount from outside: %w", err)
		}
		if errors.Is(err, syscall.ENODEV) || errors.Is(err, syscall.ENOENT) {
			return "", fmt.Errorf("mount: the kernel has no NFS client "+
				"(CONFIG_NFS_FS / nfs module missing): %w", err)
		}
		return "", fmt.Errorf("mount: mount(2) %s: %w", target, err)
	}
	return target, nil
}

// nfsDetach unmounts target, falling back to a lazy detach if it is busy.
func nfsDetach(target string) error {
	if err := syscall.Unmount(target, 0); err != nil {
		if err := syscall.Unmount(target, syscall.MNT_DETACH); err != nil {
			return fmt.Errorf("mount: unmount %s: %w", target, err)
		}
	}
	os.Remove(target) // best effort; fails harmlessly if non-empty
	return nil
}

// mountRoot picks where per-domain mountpoints live: the user's runtime dir
// when available, otherwise /run or /tmp.
func mountRoot() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "simplefs")
	}
	if os.Getuid() == 0 {
		return "/run/simplefs"
	}
	return filepath.Join(os.TempDir(), "simplefs")
}
