package mount

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// attachNFS mounts the loopback server with /sbin/mount_nfs. macOS lets a
// user mount NFS on a directory they own, so no root, app bundle, or signing
// is needed. noresvport lets an unprivileged server port be used; the soft
// mount fails operations instead of hanging if this process dies, and
// deadtimeout makes the kernel force-unmount it 30s after that.
func attachNFS(ctx context.Context, cfg attachConfig) (string, func(), error) {
	target, cleanup, err := darwinPath(cfg)
	if err != nil {
		return "", nil, err
	}
	opts := fmt.Sprintf("vers=3,tcp,port=%d,mountport=%d,soft,timeo=50,retrans=1,"+
		"deadtimeout=30,noresvport,locallocks,nobrowse", cfg.port, cfg.port)
	if cfg.readOnly {
		opts += ",rdonly"
	}
	out, err := exec.CommandContext(ctx, "/sbin/mount_nfs", "-o", opts, "127.0.0.1:/", target).CombinedOutput()
	if err != nil {
		cleanup()
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "ermission denied") || strings.Contains(msg, "not permitted") {
			return "", nil, fmt.Errorf("%w: mount_nfs refused (%s); mount on a directory you own, or "+
				"run: sudo mount_nfs -o %s 127.0.0.1:/ %s", ErrNoPrivilege, msg, opts, target)
		}
		return "", nil, fmt.Errorf("mount: mount_nfs %s: %s: %w", target, msg, err)
	}
	return target, cleanup, nil
}

func darwinPath(cfg attachConfig) (string, func(), error) {
	if cfg.path != "" {
		abs, err := filepath.Abs(cfg.path)
		if err != nil {
			return "", nil, fmt.Errorf("mount: %w", err)
		}
		cleanup, err := ensureDir(abs)
		return abs, cleanup, err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	return autoDir(filepath.Join(cache, "simplefs"), cfg.name)
}

// detachNFS unmounts target, forcing it if it is busy.
func detachNFS(target string) error {
	out, err := exec.Command("/sbin/umount", target).CombinedOutput()
	if err == nil || strings.Contains(string(out), "not currently mounted") {
		return nil
	}
	if out2, err2 := exec.Command("/usr/sbin/diskutil", "unmount", "force", target).CombinedOutput(); err2 != nil {
		return fmt.Errorf("mount: unmount %s: %s / %s: %w",
			target, strings.TrimSpace(string(out)), strings.TrimSpace(string(out2)), err)
	}
	return nil
}
