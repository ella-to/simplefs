//go:build !linux && !windows

package mount

import "errors"

// ErrUnsupported is returned by the domain-management functions on platforms
// where no mount backend exists yet. The Bridge itself is pure Go and works
// everywhere; only mounting is platform-specific.
var ErrUnsupported = errors.New("mount: no mount backend for this platform (Linux/Windows NFS)")

// Mount is unsupported on this platform.
func Mount(domainID, displayName string) (string, error) { return "", ErrUnsupported }

// Unmount is unsupported on this platform.
func Unmount(domainID string) error { return ErrUnsupported }

// List is unsupported on this platform.
func List() ([]Domain, error) { return nil, ErrUnsupported }

// UnmountAll is unsupported on this platform.
func UnmountAll() error { return ErrUnsupported }
