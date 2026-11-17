package nfs

// Loopback benchmarks: a real client speaks RPC over TCP to a Server, so
// numbers include XDR, record marking and the provider. "slow" wraps memfs
// so every FileProvider call costs 1 ms, like a remote backend; calls/op
// counts FileProvider calls per benchmark op.
//
//	go test -run '^$' -bench . -benchmem ./nfs

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"ella.to/simplefs"
	"ella.to/simplefs/internal/bufpool"
	"ella.to/simplefs/memfs"
)

// countingFS counts FileProvider calls and optionally delays each one.
type countingFS struct {
	*memfs.FS
	delay time.Duration
	calls atomic.Int64
}

func (c *countingFS) tick() {
	c.calls.Add(1)
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
}

func (c *countingFS) Unwrap() simplefs.FileProvider { return c.FS }

func (c *countingFS) Stat(ctx context.Context, p string) (simplefs.FileInfo, error) {
	c.tick()
	return c.FS.Stat(ctx, p)
}

func (c *countingFS) Open(ctx context.Context, p string, o simplefs.OpenOptions) (simplefs.FileHandle, error) {
	c.tick()
	return c.FS.Open(ctx, p, o)
}

func (c *countingFS) ReadDir(ctx context.Context, p string, pg simplefs.Page) (simplefs.DirPage, error) {
	c.tick()
	return c.FS.ReadDir(ctx, p, pg)
}

func (c *countingFS) Mkdir(ctx context.Context, p string, o simplefs.MkdirOptions) error {
	c.tick()
	return c.FS.Mkdir(ctx, p, o)
}

func (c *countingFS) Remove(ctx context.Context, p string, o simplefs.RemoveOptions) error {
	c.tick()
	return c.FS.Remove(ctx, p, o)
}

func (c *countingFS) Rename(ctx context.Context, a, b string, o simplefs.RenameOptions) error {
	c.tick()
	return c.FS.Rename(ctx, a, b, o)
}

var benchProviders = []struct {
	name  string
	delay time.Duration
}{
	{"mem", 0},
	{"slow", time.Millisecond},
}

func benchServer(b *testing.B, delay time.Duration) (*countingFS, *testClient, []byte) {
	b.Helper()
	fs := &countingFS{FS: memfs.New(memfs.WithCapacity(4 << 30)), delay: delay}
	_, c := startServerWith(b, fs, Options{AttrCacheTTL: time.Second})
	return fs, c, c.mnt()
}

// report adds provider-calls per op.
func report(b *testing.B, fs *countingFS, before int64) {
	b.ReportMetric(float64(fs.calls.Load()-before)/float64(b.N), "calls/op")
}

func BenchmarkWrite1MiB(b *testing.B) {
	for _, bp := range benchProviders {
		b.Run(bp.name, func(b *testing.B) {
			fs, c, root := benchServer(b, bp.delay)
			fh := c.create(root, "big")
			chunk := make([]byte, 1<<20)
			b.SetBytes(int64(len(chunk)))
			before := fs.calls.Load()
			b.ResetTimer()
			for i := range b.N {
				off := uint64(i%256) << 20
				args := newWriter()
				args.opaque(fh)
				args.uint64(off)
				args.uint32(uint32(len(chunk)))
				args.uint32(writeUnstable)
				args.opaque(chunk)
				r := c.nfsCall(nfsProcWrite, args, nfsOK)
				skipWcc(r)
			}
			report(b, fs, before)
		})
	}
}

func BenchmarkRead1MiB(b *testing.B) {
	for _, bp := range benchProviders {
		b.Run(bp.name, func(b *testing.B) {
			fs, c, root := benchServer(b, bp.delay)
			fh := c.create(root, "big")
			chunk := make([]byte, 1<<20)
			for i := range 64 {
				c.write(fh, uint64(i)<<20, chunk)
			}
			b.SetBytes(int64(len(chunk)))
			before := fs.calls.Load()
			b.ResetTimer()
			for i := range b.N {
				c.read(fh, uint64(i%64)<<20, 1<<20)
			}
			report(b, fs, before)
		})
	}
}

func BenchmarkCreate(b *testing.B) {
	for _, bp := range benchProviders {
		b.Run(bp.name, func(b *testing.B) {
			fs, c, root := benchServer(b, bp.delay)
			before := fs.calls.Load()
			b.ResetTimer()
			for i := range b.N {
				c.create(root, "f"+strconv.Itoa(i))
			}
			report(b, fs, before)
		})
	}
}

// BenchmarkStatStorm is what `ls -l` and editors do to one file: LOOKUP,
// GETATTR and ACCESS over and over.
func BenchmarkStatStorm(b *testing.B) {
	for _, bp := range benchProviders {
		b.Run(bp.name, func(b *testing.B) {
			fs, c, root := benchServer(b, bp.delay)
			fh := c.create(root, "f")
			before := fs.calls.Load()
			b.ResetTimer()
			for range b.N {
				c.lookup(root, "f", nfsOK)
				args := newWriter()
				args.opaque(fh)
				c.nfsCall(nfsProcGetattr, args, nfsOK)
				args = newWriter()
				args.opaque(fh)
				args.uint32(accessRead)
				c.nfsCall(nfsProcAccess, args, nfsOK)
			}
			report(b, fs, before)
		})
	}
}

// BenchmarkReaddirplus lists a 50k-entry directory completely per op, in
// 32 KiB replies like the Linux client.
func BenchmarkReaddirplus(b *testing.B) {
	const entries = 50_000
	for _, bp := range benchProviders {
		b.Run(bp.name, func(b *testing.B) {
			fs, c, root := benchServer(b, 0)
			for i := range entries {
				h, _ := fs.FS.Open(b.Context(), "/f"+strconv.Itoa(i), simplefs.OpenOptions{Write: true, Create: true})
				h.Close()
			}
			fs.delay = bp.delay
			before := fs.calls.Load()
			b.ResetTimer()
			for range b.N {
				var cookie uint64
				verf := make([]byte, 8)
				for {
					args := newWriter()
					args.opaque(root)
					args.uint64(cookie)
					args.raw(verf)
					args.uint32(8192)
					args.uint32(32768)
					r := c.nfsCall(nfsProcReaddirplus, args, nfsOK)
					skipPostOpAttr(r)
					verf = append(verf[:0], r.bytesN(8)...)
					n := 0
					for r.bool() {
						r.uint64()
						r.string()
						cookie = r.uint64()
						skipPostOpAttr(r)
						if r.bool() {
							r.opaque()
						}
						n++
					}
					if r.bool() || n == 0 {
						break
					}
				}
			}
			report(b, fs, before)
		})
	}
}

// BenchmarkDispatch measures the server alone (no TCP, no client): one
// 1 MiB READ or WRITE through dispatch, with the record in a pooled buffer
// as serveConn provides it.
func BenchmarkDispatch(b *testing.B) {
	p := memfs.New(memfs.WithCapacity(4 << 30))
	simplefs.WriteFile(b.Context(), p, "/f", make([]byte, 64<<20), 0)
	srv := NewServer(p, Options{AttrCacheTTL: time.Second})
	defer srv.Close()
	id := srv.root().fh.ID("/f")
	fh := srv.root().encodeFH(id)

	read := newWriter()
	read.opaque(fh)
	read.uint64(0)
	read.uint32(1 << 20)
	write := newWriter()
	write.opaque(fh)
	write.uint64(0)
	write.uint32(1 << 20)
	write.uint32(writeUnstable)
	write.opaque(make([]byte, 1<<20))

	for _, tc := range []struct {
		name string
		proc uint32
		args *xdrWriter
	}{{"read", nfsProcRead, read}, {"write", nfsProcWrite, write}} {
		b.Run(tc.name, func(b *testing.B) {
			msg := rpcMsg(progNFS, 3, tc.proc, tc.args)
			b.SetBytes(1 << 20)
			b.ReportAllocs()
			for b.Loop() {
				rec := bufpool.Get(len(msg))
				copy(rec, msg)
				reply := srv.dispatch(b.Context(), "bench", rec)
				bufpool.Put(rec)
				reply.release()
			}
		})
	}
}
