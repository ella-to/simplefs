package mount

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"ella.to/simplefs"
	"ella.to/simplefs/webdav"
)

// mountWebDAV serves p over WebDAV on a loopback port and attaches it with
// the OS WebDAV client (attachWebDAV, per platform).
func mountWebDAV(ctx context.Context, p simplefs.FileProvider, opts Options) (*Point, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("mount: start WebDAV server: %w", err)
	}
	srv := &http.Server{
		Handler:           webdav.NewHandler(p, webdav.Options{ReadOnly: opts.ReadOnly, Logger: opts.Logger}),
		ReadHeaderTimeout: 30 * time.Second,
	}
	go srv.Serve(ln)
	stop := func(ctx context.Context) error {
		if err := srv.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}

	url := fmt.Sprintf("http://127.0.0.1:%d/", ln.Addr().(*net.TCPAddr).Port)
	target, cleanup, err := attachWebDAV(ctx, url, attachConfig{path: opts.Path, name: opts.Name, readOnly: opts.ReadOnly})
	if err != nil {
		stop(context.Background())
		return nil, err
	}
	opts.Logger.Info("mount: mounted", "name", opts.Name, "path", target, "backend", "webdav", "url", url)
	return &Point{
		path:    target,
		name:    opts.Name,
		log:     opts.Logger,
		stop:    stop,
		detach:  func() error { return detachWebDAV(target) },
		cleanup: cleanup,
		done:    make(chan struct{}),
	}, nil
}
