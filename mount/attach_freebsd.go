package mount

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// attachNFS mounts the loopback server with mount_nfs (needs root unless
// vfs.usermount is enabled).
func attachNFS(ctx context.Context, cfg attachConfig) (string, func(), error) {
	target, cleanup, err := bsdPath(cfg)
	if err != nil {
		return "", nil, err
	}
	opts := fmt.Sprintf("nfsv3,tcp,port=%d,mountport=%d,nolockd,soft,timeout=50,retrans=1", cfg.port, cfg.port)
	if cfg.readOnly {
		opts += ",ro"
	}
	out, err := exec.CommandContext(ctx, "mount_nfs", "-o", opts, "127.0.0.1:/", target).CombinedOutput()
	if err != nil {
		cleanup()
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "ermission denied") || strings.Contains(msg, "not permitted") {
			return "", nil, fmt.Errorf("%w: mount_nfs needs root (or sysctl vfs.usermount=1): %s", ErrNoPrivilege, msg)
		}
		return "", nil, fmt.Errorf("mount: mount_nfs %s: %s: %w", target, msg, err)
	}
	return target, cleanup, nil
}

func bsdPath(cfg attachConfig) (string, func(), error) {
	if cfg.path != "" {
		abs, err := filepath.Abs(cfg.path)
		if err != nil {
			return "", nil, fmt.Errorf("mount: %w", err)
		}
		cleanup, err := ensureDir(abs)
		return abs, cleanup, err
	}
	return autoDir(filepath.Join(os.TempDir(), "simplefs"), cfg.name)
}

func detachNFS(target string) error {
	if out, err := exec.Command("umount", target).CombinedOutput(); err != nil {
		if out2, err2 := exec.Command("umount", "-f", target).CombinedOutput(); err2 != nil {
			return fmt.Errorf("mount: umount %s: %s / %s: %w",
				target, strings.TrimSpace(string(out)), strings.TrimSpace(string(out2)), err)
		}
	}
	return nil
}

// CleanupStale is a no-op on FreeBSD.
func CleanupStale(ctx context.Context) error { return nil }
