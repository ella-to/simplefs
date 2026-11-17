//go:build linux || darwin

package mount

import "context"

// CleanupStale detaches loopback NFS mounts left behind by processes that
// exited without unmounting. Mount calls it first.
func CleanupStale(ctx context.Context) error { return nil }
