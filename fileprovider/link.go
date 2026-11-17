package fileprovider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// AddDomainAt adds the domain like AddDomain and then exposes it at
// linkPath.
//
// macOS — not the caller — chooses where a File Provider domain really lives
// (under ~/Library/CloudStorage), so AddDomainAt creates a symlink from
// linkPath to that location; the folder then looks and behaves as if it were
// mounted at linkPath. The real path is returned.
//
// Missing parent directories of linkPath are created. An existing symlink at
// linkPath is replaced (it is ours from a previous run); anything else at
// linkPath is an error rather than something to overwrite. An empty linkPath
// behaves exactly like AddDomain.
func AddDomainAt(ctx context.Context, domainID, displayName, linkPath string) (realPath string, err error) {
	realPath, err = AddDomain(ctx, domainID, displayName)
	if err != nil {
		return "", err
	}
	if linkPath == "" {
		return realPath, nil
	}
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		return "", fmt.Errorf("fileprovider: create parent of %s: %w", linkPath, err)
	}
	if fi, err := os.Lstat(linkPath); err == nil {
		if fi.Mode()&os.ModeSymlink == 0 {
			return "", fmt.Errorf("fileprovider: %s already exists and is not a symlink", linkPath)
		}
		if err := os.Remove(linkPath); err != nil {
			return "", fmt.Errorf("fileprovider: replace symlink %s: %w", linkPath, err)
		}
	}
	if err := os.Symlink(realPath, linkPath); err != nil {
		return "", fmt.Errorf("fileprovider: link %s -> %s: %w", linkPath, realPath, err)
	}
	return realPath, nil
}

// RemoveDomainAt removes the domain like RemoveDomain and deletes the
// symlink AddDomainAt created at linkPath. A missing symlink is a no-op;
// anything at linkPath that is not a symlink is left alone.
func RemoveDomainAt(ctx context.Context, domainID, linkPath string) error {
	if linkPath != "" {
		if fi, err := os.Lstat(linkPath); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(linkPath); err != nil {
				return fmt.Errorf("fileprovider: remove symlink %s: %w", linkPath, err)
			}
		}
	}
	return RemoveDomain(ctx, domainID)
}
