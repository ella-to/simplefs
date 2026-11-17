// Command wrappers shows provider middleware: the built-in simplefs.ReadOnly
// and simplefs.Sub views, and a custom logging/metrics layer that forwards
// to an inner provider while keeping its optional capabilities reachable via
// Unwrap and simplefs.As.
//
//	go run ./examples/wrappers
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"ella.to/simplefs"
	"ella.to/simplefs/memfs"
)

// metrics wraps a provider, logging and counting every core call.
//
// It implements Unwrap, so simplefs.As finds the inner provider's
// capabilities (symlinks, xattrs, ...) without metrics re-implementing them.
// That is only correct because metrics changes nothing about the calls: a
// wrapper that rewrites paths or refuses operations (like Sub or ReadOnly)
// must implement every capability itself and must not expose Unwrap.
type metrics struct {
	simplefs.FileProvider // forwards the methods it does not override
	log                   *slog.Logger
	calls, failures       atomic.Int64
}

func (m *metrics) Unwrap() simplefs.FileProvider { return m.FileProvider }

func (m *metrics) observe(op, path string, start time.Time, err error) {
	m.calls.Add(1)
	if err != nil && !errors.Is(err, simplefs.ErrNotExist) {
		m.failures.Add(1)
	}
	m.log.Info(op, "path", path, "took", time.Since(start), "err", err)
}

func (m *metrics) Stat(ctx context.Context, path string) (fi simplefs.FileInfo, err error) {
	defer func(t time.Time) { m.observe("stat", path, t, err) }(time.Now())
	return m.FileProvider.Stat(ctx, path)
}

func (m *metrics) Open(ctx context.Context, path string, o simplefs.OpenOptions) (h simplefs.FileHandle, err error) {
	defer func(t time.Time) { m.observe("open", path, t, err) }(time.Now())
	return m.FileProvider.Open(ctx, path, o)
}

func (m *metrics) Mkdir(ctx context.Context, path string, o simplefs.MkdirOptions) (err error) {
	defer func(t time.Time) { m.observe("mkdir", path, t, err) }(time.Now())
	return m.FileProvider.Mkdir(ctx, path, o)
}

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "wrappers:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	inner := memfs.New()
	m := &metrics{
		FileProvider: inner,
		log:          slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{ReplaceAttr: dropTime})),
	}
	var p simplefs.FileProvider = m

	fmt.Println("== a logging layer")
	simplefs.MkdirAll(ctx, p, "/tenants/acme/docs", 0)
	simplefs.MkdirAll(ctx, p, "/tenants/globex", 0)
	simplefs.WriteFile(ctx, p, "/tenants/acme/docs/plan.txt", []byte("plan"), 0)
	simplefs.WriteFile(ctx, p, "/tenants/globex/secret.txt", nil, 0)

	fmt.Println("== capabilities still reachable through it (Unwrap + As)")
	sp, ok := simplefs.As[simplefs.SymlinkProvider](p)
	fmt.Println("SymlinkProvider found:", ok)
	if ok {
		sp.Symlink(ctx, "docs/plan.txt", "/tenants/acme/latest")
	}

	fmt.Println("== Sub: one tenant's view")
	acme := simplefs.Sub(p, "/tenants/acme")
	info, err := acme.Stat(ctx, "/docs/plan.txt")
	if err != nil {
		return err
	}
	fmt.Println("acme sees", info.Path)
	_, err = acme.Stat(ctx, "/../globex/secret.txt") // ".." stops at the view's root
	fmt.Println("escape attempt:", err)

	fmt.Println("== ReadOnly on top")
	ro := simplefs.ReadOnly(acme)
	data, _ := simplefs.ReadFile(ctx, ro, "/docs/plan.txt")
	fmt.Printf("read: %q\n", data)
	err = simplefs.WriteFile(ctx, ro, "/docs/plan.txt", []byte("changed"), 0)
	fmt.Println("write:", err, "| ErrReadOnly:", errors.Is(err, simplefs.ErrReadOnly))
	// ReadOnly implements the capabilities itself, so As cannot be used to
	// sneak a write past it:
	if cp, ok := simplefs.As[simplefs.ChmodProvider](ro); ok {
		fmt.Println("chmod through ReadOnly:", cp.Chmod(ctx, "/docs/plan.txt", 0o777))
	}

	fmt.Printf("== metrics: %d calls, %d failures\n", m.calls.Load(), m.failures.Load())
	return nil
}

func dropTime(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return a
}
