//go:build !darwin && !windows

package mount

import (
	"context"
	"fmt"
)

// attachWebDAV is unavailable: Linux and BSD WebDAV clients (davfs2,
// GVfs) are not part of the base system. Use the NFS backend, or serve with
// package webdav and mount with davfs2 yourself.
func attachWebDAV(ctx context.Context, url string, cfg attachConfig) (string, func(), error) {
	return "", nil, fmt.Errorf("%w: no built-in WebDAV client on this OS; use Backend: NFS "+
		"or mount %s with davfs2", ErrNoClient, url)
}

func detachWebDAV(string) error { return ErrUnsupported }
