package mount

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// attachWebDAV mounts url with /sbin/mount_webdav (-S: never show UI).
func attachWebDAV(ctx context.Context, url string, cfg attachConfig) (string, func(), error) {
	target, cleanup, err := darwinPath(cfg)
	if err != nil {
		return "", nil, err
	}
	args := []string{"-S", "-v", cfg.name}
	if cfg.readOnly {
		args = append(args, "-o", "rdonly")
	}
	args = append(args, url, target)
	out, err := exec.CommandContext(ctx, "/sbin/mount_webdav", args...).CombinedOutput()
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("mount: mount_webdav %s: %s: %w", target, strings.TrimSpace(string(out)), err)
	}
	return target, cleanup, nil
}

func detachWebDAV(target string) error { return detachNFS(target) }
