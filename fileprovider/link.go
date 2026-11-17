package mount

import (
	"fmt"
	"os"
	"path/filepath"
)

// MountAt mounts the domain like Mount and then exposes it at linkPath.
//
// macOS — not the caller — chooses where a File Provider domain really lives
// (under ~/Library/CloudStorage), so MountAt creates a symlink from linkPath
// to that location; the folder then looks and behaves as if it were mounted
// at linkPath. The real path is returned.
//
// Missing parent directories of linkPath are created. An existing symlink at
// linkPath is replaced (it is ours from a previous run); anything else at
// linkPath is an error rather than something to overwrite. An empty linkPath
// behaves exactly like Mount.
func MountAt(domainID, displayName, linkPath string) (realPath string, err error) {
	realPath, err = Mount(domainID, displayName)
	if err != nil {
		return "", err
	}
	if linkPath == "" {
		return realPath, nil
	}
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		return "", fmt.Errorf("mount: create parent of %s: %w", linkPath, err)
	}
	if fi, err := os.Lstat(linkPath); err == nil {
		if fi.Mode()&os.ModeSymlink == 0 {
			return "", fmt.Errorf("mount: %s already exists and is not a symlink", linkPath)
		}
		if err := os.Remove(linkPath); err != nil {
			return "", fmt.Errorf("mount: replace symlink %s: %w", linkPath, err)
		}
	}
	if err := os.Symlink(realPath, linkPath); err != nil {
		return "", fmt.Errorf("mount: link %s -> %s: %w", linkPath, realPath, err)
	}
	return realPath, nil
}

// UnmountAt removes the mount like Unmount and deletes the symlink MountAt
// created at linkPath. A missing symlink is a no-op; anything at linkPath
// that is not a symlink is left alone.
func UnmountAt(domainID, linkPath string) error {
	if linkPath != "" {
		if fi, err := os.Lstat(linkPath); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(linkPath); err != nil {
				return fmt.Errorf("mount: remove symlink %s: %w", linkPath, err)
			}
		}
	}
	return Unmount(domainID)
}
