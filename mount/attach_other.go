//go:build !linux && !darwin && !windows && !freebsd

package mount

import "context"

func attachNFS(ctx context.Context, cfg attachConfig) (string, func(), error) {
	return "", nil, ErrUnsupported
}

func detachNFS(string) error { return ErrUnsupported }

// CleanupStale is a no-op where mounting is unsupported.
func CleanupStale(ctx context.Context) error { return nil }
