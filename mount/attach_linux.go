package mount

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// attachNFS mounts the loopback server at cfg.path with the raw mount(2)
// syscall. With string options (vers=3 + port/mountport) the kernel performs
// the MOUNT-protocol handshake itself, so no userspace NFS tooling needs to
// exist — important inside minimal containers.
//
// The mount is soft with a short timeout: if this process dies, operations
// on the mount fail (EIO) after ~15s instead of hanging forever.
func attachNFS(ctx context.Context, cfg attachConfig) (string, func(), error) {
	target, cleanup, err := linuxPath(cfg)
	if err != nil {
		return "", nil, err
	}
	opts := fmt.Sprintf("vers=3,proto=tcp,mountproto=tcp,port=%d,mountport=%d,addr=127.0.0.1,"+
		"nolock,soft,timeo=50,retrans=1,actimeo=1", cfg.port, cfg.port)
	var flags uintptr
	if cfg.readOnly {
		flags |= syscall.MS_RDONLY
	}
	if err := syscall.Mount("127.0.0.1:/", target, "nfs", flags, opts); err != nil {
		cleanup()
		switch {
		case errors.Is(err, syscall.EPERM):
			return "", nil, fmt.Errorf("%w: mounting needs CAP_SYS_ADMIN (run as root, or "+
				"docker run --cap-add SYS_ADMIN); without it, serve with nfs.Server and mount "+
				"from outside: %v", ErrNoPrivilege, err)
		case errors.Is(err, syscall.ENODEV):
			return "", nil, fmt.Errorf("%w: the kernel has no NFS client (CONFIG_NFS_FS / "+
				"modprobe nfs): %v", ErrNoClient, err)
		}
		return "", nil, fmt.Errorf("mount: mount(2) %s: %w", target, err)
	}
	return target, cleanup, nil
}

func linuxPath(cfg attachConfig) (string, func(), error) {
	if cfg.path != "" {
		abs, err := filepath.Abs(cfg.path)
		if err != nil {
			return "", nil, fmt.Errorf("mount: %w", err)
		}
		cleanup, err := ensureDir(abs)
		return abs, cleanup, err
	}
	base := filepath.Join(os.TempDir(), "simplefs")
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		base = filepath.Join(dir, "simplefs")
	} else if os.Getuid() == 0 {
		base = "/run/simplefs"
	}
	return autoDir(base, cfg.name)
}

// detachNFS unmounts target, falling back to a lazy detach if it is busy.
func detachNFS(target string) error {
	if err := syscall.Unmount(target, 0); err != nil {
		if errors.Is(err, syscall.EINVAL) {
			return nil // not mounted (any more)
		}
		if err := syscall.Unmount(target, syscall.MNT_DETACH); err != nil {
			return fmt.Errorf("mount: unmount %s: %w", target, err)
		}
	}
	return nil
}
