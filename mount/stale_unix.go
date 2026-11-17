//go:build linux || darwin

package mount

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"
)

// staleMount is a loopback NFSv3 mount of ours: where it is mounted and the
// port its server listened on.
type staleMount struct {
	path string
	port int
}

// CleanupStale detaches loopback NFS mounts left behind by processes that
// exited without unmounting: mounts of "127.0.0.1:/" (NFSv3) whose server
// port no longer accepts connections. Mount calls it first. Other NFS mounts
// are never touched, and detaching may need the same privileges mounting
// did; errors for individual mounts are joined.
func CleanupStale(ctx context.Context) error {
	mounts, err := loopbackMounts(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, m := range mounts {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if serverAlive(ctx, m.port) {
			continue
		}
		if err := detachNFS(m.path); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// serverAlive reports whether something still accepts connections on the
// loopback port (a live server, or a new process reusing the port — either
// way the mount must be left alone).
func serverAlive(ctx context.Context, port int) bool {
	d := net.Dialer{Timeout: 300 * time.Millisecond}
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// optPort extracts port=N from a comma-separated option list.
func optPort(opts string) (int, bool) {
	for o := range strings.SplitSeq(opts, ",") {
		if v, ok := strings.CutPrefix(o, "port="); ok {
			n, err := strconv.Atoi(v)
			return n, err == nil && n > 0
		}
	}
	return 0, false
}
