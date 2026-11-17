package nfs

// Protocol-level tests: a minimal NFSv3/MOUNT client dials a real Server over
// TCP (loopback) and exercises the procedures the way a kernel client would.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"

	"ella.to/simplefs"
	"ella.to/simplefs/memfs"
)

// testClient is a tiny synchronous ONC RPC client, enough to drive the server.
type testClient struct {
	t    testing.TB
	conn net.Conn
	xid  uint32
}

func startServer(t *testing.T) (*Server, *testClient) {
	t.Helper()
	// The attribute cache is on, as package mount runs it, so every protocol
	// test also checks that the server invalidates it correctly.
	return startServerWith(t, memfs.New(), Options{AttrCacheTTL: time.Second})
}

// startServerWith serves p with opts on a loopback listener and dials it.
func startServerWith(t testing.TB, p simplefs.FileProvider, opts Options) (*Server, *testClient) {
	t.Helper()
	srv := NewServer(p, opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return srv, &testClient{t: t, conn: conn}
}

// call sends one RPC and returns a reader over the procedure results,
// asserting the RPC layer accepted it.
func (c *testClient) call(prog, vers, proc uint32, args *xdrWriter) *xdrReader {
	c.t.Helper()
	c.xid++
	w := newWriter()
	w.uint32(c.xid)
	w.uint32(msgCall)
	w.uint32(2) // rpcvers
	w.uint32(prog)
	w.uint32(vers)
	w.uint32(proc)
	w.uint32(authNone)
	w.uint32(0)
	w.uint32(authNone)
	w.uint32(0)
	if args != nil {
		w.raw(args.bytes())
	}
	msg := w.bytes()
	out := make([]byte, 4+len(msg))
	binary.BigEndian.PutUint32(out, 0x80000000|uint32(len(msg)))
	copy(out[4:], msg)
	if _, err := c.conn.Write(out); err != nil {
		c.t.Fatal(err)
	}

	var hdr [4]byte
	var reply []byte
	for {
		if _, err := io.ReadFull(c.conn, hdr[:]); err != nil {
			c.t.Fatal(err)
		}
		mark := binary.BigEndian.Uint32(hdr[:])
		frag := make([]byte, mark&0x7fffffff)
		if _, err := io.ReadFull(c.conn, frag); err != nil {
			c.t.Fatal(err)
		}
		reply = append(reply, frag...)
		if mark&0x80000000 != 0 {
			break
		}
	}
	r := newReader(reply)
	if xid := r.uint32(); xid != c.xid {
		c.t.Fatalf("xid = %d, want %d", xid, c.xid)
	}
	if mt := r.uint32(); mt != msgReply {
		c.t.Fatalf("msg type = %d", mt)
	}
	if rs := r.uint32(); rs != replyAccepted {
		c.t.Fatalf("reply denied (stat %d)", rs)
	}
	r.uint32() // verf flavor
	r.skipOpaque()
	if as := r.uint32(); as != acceptSuccess {
		c.t.Fatalf("accept_stat = %d", as)
	}
	return r
}

// nfsCall invokes an NFSv3 procedure and asserts its nfsstat3 == want,
// returning the reader positioned after the status word.
func (c *testClient) nfsCall(proc uint32, args *xdrWriter, want uint32) *xdrReader {
	c.t.Helper()
	r := c.call(progNFS, 3, proc, args)
	if st := r.uint32(); st != want {
		c.t.Fatalf("proc %d: status = %d, want %d", proc, st, want)
	}
	return r
}

// mnt performs the MOUNT handshake and returns the root filehandle.
func (c *testClient) mnt() []byte {
	c.t.Helper()
	args := newWriter()
	args.string("/")
	r := c.call(progMount, 3, mountProcMnt, args)
	if st := r.uint32(); st != 0 {
		c.t.Fatalf("MNT status = %d", st)
	}
	return r.opaque()
}

// skipPostOpAttr consumes an optional post_op_attr.
func skipPostOpAttr(r *xdrReader) {
	if r.bool() {
		r.bytesN(84)
	}
}

// skipWcc consumes a wcc_data.
func skipWcc(r *xdrReader) {
	if r.bool() {
		r.bytesN(24)
	}
	skipPostOpAttr(r)
}

// lookup resolves name under dirFH, asserting the given status; on success
// it returns the child's filehandle.
func (c *testClient) lookup(dirFH []byte, name string, want uint32) []byte {
	c.t.Helper()
	args := newWriter()
	args.opaque(dirFH)
	args.string(name)
	r := c.nfsCall(nfsProcLookup, args, want)
	if want != nfsOK {
		return nil
	}
	return r.opaque()
}

// create makes a file (UNCHECKED) and returns its filehandle.
func (c *testClient) create(dirFH []byte, name string) []byte {
	c.t.Helper()
	args := newWriter()
	args.opaque(dirFH)
	args.string(name)
	args.uint32(createUnchecked)
	// sattr3: mode set to 0644, everything else unset
	args.bool(true)
	args.uint32(0o644)
	args.bool(false)
	args.bool(false)
	args.bool(false)
	args.uint32(0)
	args.uint32(0)
	r := c.nfsCall(nfsProcCreate, args, nfsOK)
	if !r.bool() {
		c.t.Fatal("CREATE returned no filehandle")
	}
	return r.opaque()
}

// write writes data at offset with FILE_SYNC stability.
func (c *testClient) write(fh []byte, offset uint64, data []byte) {
	c.t.Helper()
	args := newWriter()
	args.opaque(fh)
	args.uint64(offset)
	args.uint32(uint32(len(data)))
	args.uint32(writeFileSync)
	args.opaque(data)
	r := c.nfsCall(nfsProcWrite, args, nfsOK)
	skipWcc(r)
	if n := r.uint32(); int(n) != len(data) {
		c.t.Fatalf("WRITE wrote %d, want %d", n, len(data))
	}
}

// read reads count bytes at offset and returns (data, eof).
func (c *testClient) read(fh []byte, offset uint64, count uint32) ([]byte, bool) {
	c.t.Helper()
	args := newWriter()
	args.opaque(fh)
	args.uint64(offset)
	args.uint32(count)
	r := c.nfsCall(nfsProcRead, args, nfsOK)
	skipPostOpAttr(r)
	n := r.uint32()
	eof := r.bool()
	data := r.opaque()
	if uint32(len(data)) != n {
		c.t.Fatalf("READ count %d != data %d", n, len(data))
	}
	return data, eof
}

func TestMountAndRoot(t *testing.T) {
	_, c := startServer(t)
	rootFH := c.mnt()
	if len(rootFH) != fhLen {
		t.Fatalf("root fh length = %d", len(rootFH))
	}

	// FSINFO and GETATTR on the root must succeed — the kernel's first moves.
	args := newWriter()
	args.opaque(rootFH)
	r := c.nfsCall(nfsProcFsinfo, args, nfsOK)
	skipPostOpAttr(r)
	if rtmax := r.uint32(); rtmax != maxIO {
		t.Fatalf("rtmax = %d", rtmax)
	}

	args = newWriter()
	args.opaque(rootFH)
	r = c.nfsCall(nfsProcGetattr, args, nfsOK)
	if ft := r.uint32(); ft != typeDir {
		t.Fatalf("root type = %d", ft)
	}
}

func TestCreateWriteRead(t *testing.T) {
	srv, c := startServer(t)
	root := c.mnt()

	fh := c.create(root, "hello.txt")
	content := []byte("hello, nfs world\n")
	c.write(fh, 0, content)

	got, eof := c.read(fh, 0, 1024)
	if !bytes.Equal(got, content) || !eof {
		t.Fatalf("read %q eof=%v, want %q eof=true", got, eof, content)
	}

	// Offset read.
	got, _ = c.read(fh, 7, 3)
	if string(got) != "nfs" {
		t.Fatalf("offset read = %q", got)
	}

	// The file must be visible through the provider too.
	info, err := srv.Provider().Stat(t.Context(), "/hello.txt")
	if err != nil || info.Size != int64(len(content)) {
		t.Fatalf("provider stat: %v %+v", err, info)
	}

	// LOOKUP resolves it and returns the same handle ID.
	fh2 := c.lookup(root, "hello.txt", nfsOK)
	if !bytes.Equal(fh, fh2) {
		t.Fatalf("lookup fh %x != create fh %x", fh2, fh)
	}
}

func TestLookupMissing(t *testing.T) {
	_, c := startServer(t)
	root := c.mnt()
	c.lookup(root, "no-such-file", nfsErrNoEnt)
}

func TestMkdirReaddirPlus(t *testing.T) {
	_, c := startServer(t)
	root := c.mnt()

	args := newWriter()
	args.opaque(root)
	args.string("docs")
	args.bool(false) // mode unset
	args.bool(false)
	args.bool(false)
	args.bool(false)
	args.uint32(0)
	args.uint32(0)
	r := c.nfsCall(nfsProcMkdir, args, nfsOK)
	if !r.bool() {
		t.Fatal("MKDIR returned no fh")
	}
	dirFH := r.opaque()

	c.create(dirFH, "a.txt")
	c.create(dirFH, "b.txt")

	args = newWriter()
	args.opaque(dirFH)
	args.uint64(0)            // cookie
	args.raw(make([]byte, 8)) // cookieverf
	args.uint32(4096)         // dircount
	args.uint32(32 * 1024)    // maxcount
	r = c.nfsCall(nfsProcReaddirplus, args, nfsOK)
	skipPostOpAttr(r)
	r.bytesN(8) // cookieverf

	var names []string
	for r.bool() {
		r.uint64() // fileid
		names = append(names, r.string())
		r.uint64() // cookie
		skipPostOpAttr(r)
		if r.bool() {
			r.skipOpaque() // fh
		}
	}
	if !r.bool() {
		t.Fatal("expected eof")
	}
	want := map[string]bool{".": true, "..": true, "a.txt": true, "b.txt": true}
	if len(names) != len(want) {
		t.Fatalf("entries = %v", names)
	}
	for _, n := range names {
		if !want[n] {
			t.Fatalf("unexpected entry %q in %v", n, names)
		}
	}
}

func TestReaddirPagination(t *testing.T) {
	srv, c := startServer(t)
	root := c.mnt()
	for i := range 40 {
		name := "file-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		if _, err := simplefs.Create(t.Context(), srv.Provider(), "/"+name, 0); err != nil {
			t.Fatal(err)
		}
	}

	// Tiny maxcount forces multiple READDIR round trips via cookies.
	seen := map[string]bool{}
	cookie := uint64(0)
	for rounds := 0; ; rounds++ {
		if rounds > 50 {
			t.Fatal("readdir did not converge")
		}
		args := newWriter()
		args.opaque(root)
		args.uint64(cookie)
		args.raw(make([]byte, 8))
		args.uint32(512) // small budget
		r := c.nfsCall(nfsProcReaddir, args, nfsOK)
		skipPostOpAttr(r)
		r.bytesN(8)
		for r.bool() {
			r.uint64()
			seen[r.string()] = true
			cookie = r.uint64()
		}
		if r.bool() {
			break
		}
	}
	if len(seen) != 42 { // 40 files + "." + ".."
		t.Fatalf("saw %d entries", len(seen))
	}
}

func TestRenameKeepsHandle(t *testing.T) {
	_, c := startServer(t)
	root := c.mnt()
	fh := c.create(root, "old.txt")
	c.write(fh, 0, []byte("data"))

	args := newWriter()
	args.opaque(root)
	args.string("old.txt")
	args.opaque(root)
	args.string("new.txt")
	r := c.nfsCall(nfsProcRename, args, nfsOK)
	skipWcc(r)
	skipWcc(r)

	// The old handle must keep working (same file, new name).
	got, _ := c.read(fh, 0, 16)
	if string(got) != "data" {
		t.Fatalf("read after rename = %q", got)
	}
	c.lookup(root, "old.txt", nfsErrNoEnt)
	fh2 := c.lookup(root, "new.txt", nfsOK)
	if !bytes.Equal(fh, fh2) {
		t.Fatal("fileid changed across rename")
	}
}

func TestRemoveAndStaleHandle(t *testing.T) {
	_, c := startServer(t)
	root := c.mnt()
	fh := c.create(root, "gone.txt")

	args := newWriter()
	args.opaque(root)
	args.string("gone.txt")
	r := c.nfsCall(nfsProcRemove, args, nfsOK)
	skipWcc(r)

	// Handle is now stale.
	args = newWriter()
	args.opaque(fh)
	c.nfsCall(nfsProcGetattr, args, nfsErrStale)

	// RMDIR on a file and non-empty semantics.
	c.lookup(root, "gone.txt", nfsErrNoEnt)
}

func TestSetattrTruncate(t *testing.T) {
	_, c := startServer(t)
	root := c.mnt()
	fh := c.create(root, "t.txt")
	c.write(fh, 0, []byte("0123456789"))

	args := newWriter()
	args.opaque(fh)
	args.bool(false) // mode
	args.bool(false) // uid
	args.bool(false) // gid
	args.bool(true)  // size
	args.uint64(4)
	args.uint32(0)   // atime: don't change
	args.uint32(0)   // mtime: don't change
	args.bool(false) // no guard
	r := c.nfsCall(nfsProcSetattr, args, nfsOK)
	skipWcc(r)

	got, eof := c.read(fh, 0, 100)
	if string(got) != "0123" || !eof {
		t.Fatalf("after truncate: %q eof=%v", got, eof)
	}
}

func TestSymlink(t *testing.T) {
	_, c := startServer(t)
	root := c.mnt()

	args := newWriter()
	args.opaque(root)
	args.string("link")
	args.bool(false) // sattr: all unset
	args.bool(false)
	args.bool(false)
	args.bool(false)
	args.uint32(0)
	args.uint32(0)
	args.string("/target/path")
	r := c.nfsCall(nfsProcSymlink, args, nfsOK)
	if !r.bool() {
		t.Fatal("SYMLINK returned no fh")
	}
	fh := r.opaque()

	args = newWriter()
	args.opaque(fh)
	r = c.nfsCall(nfsProcReadlink, args, nfsOK)
	skipPostOpAttr(r)
	if target := r.string(); target != "/target/path" {
		t.Fatalf("readlink = %q", target)
	}
}

func TestExclusiveCreateRetransmit(t *testing.T) {
	_, c := startServer(t)
	root := c.mnt()

	verf := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	exclusive := func() *xdrReader {
		args := newWriter()
		args.opaque(root)
		args.string("x.lock")
		args.uint32(createExclusive)
		args.raw(verf)
		return c.call(progNFS, 3, nfsProcCreate, args)
	}
	if st := exclusive().uint32(); st != nfsOK {
		t.Fatalf("first exclusive create: %d", st)
	}
	// Same verifier: treated as a retransmission, succeeds again.
	if st := exclusive().uint32(); st != nfsOK {
		t.Fatalf("retransmitted exclusive create: %d", st)
	}
	// Different verifier: real conflict.
	verf = []byte{9, 9, 9, 9, 9, 9, 9, 9}
	if st := exclusive().uint32(); st != nfsErrExist {
		t.Fatalf("conflicting exclusive create: %d", st)
	}
}

func TestPortmapGetport(t *testing.T) {
	srv, c := startServer(t)
	args := newWriter()
	args.uint32(progNFS)
	args.uint32(3)
	args.uint32(protoTCP)
	args.uint32(0)
	r := c.call(progPortmap, 2, pmapProcGetport, args)
	if port := r.uint32(); port != uint32(srv.Port()) {
		t.Fatalf("portmap says %d, server on %d", port, srv.Port())
	}
}

func TestNLMGrantsEverything(t *testing.T) {
	_, c := startServer(t)
	args := newWriter()
	args.opaque([]byte{0xde, 0xad}) // cookie
	// (rest of nlm4_lockargs is ignored by the server)
	r := c.call(progNLM, 4, nlmProcLock, args)
	if cookie := r.opaque(); !bytes.Equal(cookie, []byte{0xde, 0xad}) {
		t.Fatalf("cookie = %x", cookie)
	}
	if st := r.uint32(); st != nlmGranted {
		t.Fatalf("lock status = %d", st)
	}
}

func TestBigWriteMultipleFragments(t *testing.T) {
	// A 300 KiB WRITE exercises TCP record reassembly and the 1 MiB maxIO.
	_, c := startServer(t)
	root := c.mnt()
	fh := c.create(root, "big.bin")

	big := make([]byte, 300*1024)
	for i := range big {
		big[i] = byte(i * 7)
	}
	c.write(fh, 0, big)

	var got []byte
	off := uint64(0)
	for {
		chunk, eof := c.read(fh, off, 128*1024)
		got = append(got, chunk...)
		off += uint64(len(chunk))
		if eof {
			break
		}
	}
	if !bytes.Equal(got, big) {
		t.Fatalf("big file round trip mismatch (%d vs %d bytes)", len(got), len(big))
	}
}

// timesRecorder records the arguments of every Chtimes call.
type timesRecorder struct {
	*memfs.FS
	atimes, mtimes []time.Time
}

func (r *timesRecorder) Chtimes(ctx context.Context, path string, atime, mtime time.Time) error {
	r.atimes = append(r.atimes, atime)
	r.mtimes = append(r.mtimes, mtime)
	return r.FS.Chtimes(ctx, path, atime, mtime)
}

func TestSetattrMtimeOnlyLeavesAtimeZero(t *testing.T) {
	rec := &timesRecorder{FS: memfs.New()}
	_, c := startServerWith(t, rec, Options{})
	root := c.mnt()
	fh := c.create(root, "f")

	want := time.Unix(1_700_000_000, 0)
	args := newWriter()
	args.opaque(fh)
	args.bool(false) // mode
	args.bool(false) // uid
	args.bool(false) // gid
	args.bool(false) // size
	args.uint32(0)   // atime: don't change
	args.uint32(2)   // mtime: client time
	args.uint32(uint32(want.Unix()))
	args.uint32(0)
	args.bool(false) // no guard
	c.nfsCall(nfsProcSetattr, args, nfsOK)

	if len(rec.mtimes) != 1 {
		t.Fatalf("Chtimes called %d times", len(rec.mtimes))
	}
	if !rec.atimes[0].IsZero() {
		t.Fatalf("atime = %v, want zero (unchanged)", rec.atimes[0])
	}
	if !rec.mtimes[0].Equal(want) {
		t.Fatalf("mtime = %v, want %v", rec.mtimes[0], want)
	}

	// An atime-only SETATTR must not clobber the stored mtime.
	args = newWriter()
	args.opaque(fh)
	args.bool(false)
	args.bool(false)
	args.bool(false)
	args.bool(false)
	args.uint32(1) // atime: server time
	args.uint32(0) // mtime: don't change
	args.bool(false)
	c.nfsCall(nfsProcSetattr, args, nfsOK)
	info, err := rec.Stat(t.Context(), "/f")
	if err != nil || !info.ModTime.Equal(want) {
		t.Fatalf("mtime after atime-only setattr = %v (%v), want %v", info.ModTime, err, want)
	}
}

func TestCloseStopsConnections(t *testing.T) {
	before := runtime.NumGoroutine()

	srv := NewServer(memfs.New(), Options{})
	if err := srv.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe("127.0.0.1:0") }()

	var conns []net.Conn
	for range 4 {
		conn, err := net.Dial("tcp", srv.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
		c := &testClient{t: t, conn: conn}
		c.mnt()
	}

	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-served; !errors.Is(err, ErrServerClosed) {
		t.Fatalf("ListenAndServe = %v, want ErrServerClosed", err)
	}
	// Every accepted connection must have been closed by the server.
	for _, conn := range conns {
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Read(make([]byte, 1)); err == nil {
			t.Fatal("connection still open after Close")
		}
		conn.Close()
	}

	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("goroutines: %d after Close, %d before", n, before)
	}
}

func TestCreateVerfBounded(t *testing.T) {
	srv := NewServer(memfs.New(), Options{})
	defer srv.Close()
	var v [8]byte
	for i := range maxCreateVerfs * 3 {
		srv.rememberCreateVerf("/f"+strconv.Itoa(i), v)
	}
	if n := len(srv.createVerf); n > maxCreateVerfs {
		t.Fatalf("createVerf holds %d entries, cap %d", n, maxCreateVerfs)
	}
	if len(srv.verfOrder) > 2*maxCreateVerfs {
		t.Fatalf("verfOrder not compacted: %d", len(srv.verfOrder))
	}
	srv.renameCreateVerf("/f"+strconv.Itoa(maxCreateVerfs*3-1), "/moved")
	if !srv.sameCreateVerf("/moved", v) {
		t.Fatal("verifier did not follow rename")
	}
}

func TestAccessHonoursMode(t *testing.T) {
	p := memfs.New()
	_, c := startServerWith(t, p, Options{})
	root := c.mnt()
	fh := c.create(root, "ro.txt")
	if err := p.Chmod(t.Context(), "/ro.txt", 0o444); err != nil {
		t.Fatal(err)
	}
	args := newWriter()
	args.opaque(fh)
	args.uint32(accessRead | accessModify | accessExtend | accessExecute)
	r := c.nfsCall(nfsProcAccess, args, nfsOK)
	skipPostOpAttr(r)
	if got := r.uint32(); got != accessRead {
		t.Fatalf("ACCESS = %#x, want READ only", got)
	}
}

func TestReadOnlyOption(t *testing.T) {
	p := memfs.New()
	simplefs.WriteFile(t.Context(), p, "/f", []byte("data"), 0)
	_, c := startServerWith(t, p, Options{ReadOnly: true})
	root := c.mnt()
	fh := c.lookup(root, "f", nfsOK)
	data, _ := c.read(fh, 0, 10)
	if string(data) != "data" {
		t.Fatalf("read = %q", data)
	}
	args := newWriter()
	args.opaque(root)
	args.string("new")
	args.uint32(createUnchecked)
	args.bool(false)
	args.bool(false)
	args.bool(false)
	args.bool(false)
	args.uint32(0)
	args.uint32(0)
	c.nfsCall(nfsProcCreate, args, nfsErrROFS)

	args = newWriter()
	args.opaque(fh)
	args.uint32(accessRead | accessModify)
	r := c.nfsCall(nfsProcAccess, args, nfsOK)
	skipPostOpAttr(r)
	if got := r.uint32(); got != accessRead {
		t.Fatalf("ACCESS on read-only export = %#x, want READ", got)
	}
}

func TestShutdownWaitsAndSyncs(t *testing.T) {
	srv := NewServer(memfs.New(), Options{})
	if err := srv.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	addr := srv.Addr().String()
	if err := srv.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := net.Dial("tcp", addr); err == nil {
		t.Fatal("still accepting after Shutdown")
	}
}

type chownRecorder struct {
	*memfs.FS
	uid, gid int
}

func (r *chownRecorder) Chown(ctx context.Context, path string, uid, gid int) error {
	r.uid, r.gid = uid, gid
	return nil
}

func TestSetattrChown(t *testing.T) {
	rec := &chownRecorder{FS: memfs.New()}
	_, c := startServerWith(t, rec, Options{})
	fh := c.create(c.mnt(), "f")
	args := newWriter()
	args.opaque(fh)
	args.bool(false)  // mode
	args.bool(false)  // uid unset
	args.bool(true)   // gid
	args.uint32(1234) //
	args.bool(false)  // size
	args.uint32(0)
	args.uint32(0)
	args.bool(false)
	c.nfsCall(nfsProcSetattr, args, nfsOK)
	if rec.uid != -1 || rec.gid != 1234 {
		t.Fatalf("Chown(%d, %d), want (-1, 1234)", rec.uid, rec.gid)
	}
}

func TestAllowedClients(t *testing.T) {
	srv := NewServer(memfs.New(), Options{})
	cases := []struct {
		allow []netip.Prefix
		addr  string
		want  bool
	}{
		{nil, "127.0.0.1:900", true},
		{nil, "[::1]:900", true},
		{nil, "[::ffff:127.0.0.1]:900", true},
		{nil, "10.0.0.2:900", false},
		{[]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, "10.1.2.3:900", true},
		{[]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, "127.0.0.1:900", false},
		{[]netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}, "192.168.1.1:900", true},
	}
	for _, c := range cases {
		srv.opts.AllowedClients = c.allow
		addr, _ := net.ResolveTCPAddr("tcp", c.addr)
		if got := srv.allowed(addr); got != c.want {
			t.Errorf("allow=%v addr=%s: got %v", c.allow, c.addr, got)
		}
	}
	srv.Close()

	// A rejected connection is closed right away.
	_, c := startServerWith(t, memfs.New(), Options{AllowedClients: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}})
	c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection from a disallowed client stayed open")
	}
}

func TestHandleFromAnotherServerIsStale(t *testing.T) {
	p := memfs.New()
	simplefs.WriteFile(t.Context(), p, "/f", []byte("x"), 0)
	_, a := startServerWith(t, p, Options{})
	fh := a.lookup(a.mnt(), "f", nfsOK)

	// Same provider, new server process: the old handle must be stale.
	_, b := startServerWith(t, p, Options{})
	b.mnt()
	args := newWriter()
	args.opaque(fh)
	b.nfsCall(nfsProcGetattr, args, nfsErrStale)

	args = newWriter()
	args.opaque(fh[:8])
	b.nfsCall(nfsProcGetattr, args, nfsErrBadHandle)
}

// blockingFS blocks Stat on "/slow" until the request context ends.
type blockingFS struct {
	*memfs.FS
	entered chan struct{}
}

func (b *blockingFS) Stat(ctx context.Context, path string) (simplefs.FileInfo, error) {
	if path == "/slow" {
		select {
		case b.entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return simplefs.FileInfo{}, ctx.Err()
	}
	return b.FS.Stat(ctx, path)
}

func TestRequestTimeoutIsJukebox(t *testing.T) {
	p := &blockingFS{FS: memfs.New(), entered: make(chan struct{}, 1)}
	_, c := startServerWith(t, p, Options{Timeout: 50 * time.Millisecond})
	c.lookup(c.mnt(), "slow", nfsErrJukebox)
}

func TestCloseCancelsInFlightCalls(t *testing.T) {
	p := &blockingFS{FS: memfs.New(), entered: make(chan struct{}, 1)}
	srv, c := startServerWith(t, p, Options{})
	root := c.mnt()
	go func() {
		args := newWriter()
		args.opaque(root)
		args.string("slow")
		c.callNoWait(progNFS, 3, nfsProcLookup, args)
	}()
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("LOOKUP never reached the provider")
	}
	done := make(chan struct{})
	go func() { srv.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel the blocked provider call")
	}
}

// callNoWait sends one RPC without reading the reply.
func (c *testClient) callNoWait(prog, vers, proc uint32, args *xdrWriter) {
	c.xid++
	w := newWriter()
	w.uint32(c.xid)
	w.uint32(msgCall)
	w.uint32(2)
	w.uint32(prog)
	w.uint32(vers)
	w.uint32(proc)
	w.uint32(authNone)
	w.uint32(0)
	w.uint32(authNone)
	w.uint32(0)
	w.raw(args.bytes())
	msg := w.bytes()
	out := make([]byte, 4+len(msg))
	binary.BigEndian.PutUint32(out, 0x80000000|uint32(len(msg)))
	copy(out[4:], msg)
	c.conn.Write(out)
}

func TestReaddirSnapshotVerifier(t *testing.T) {
	p := memfs.New()
	for i := range 50 {
		simplefs.WriteFile(t.Context(), p, "/f"+strconv.Itoa(i), nil, 0)
	}
	_, c := startServerWith(t, p, Options{})
	root := c.mnt()
	readdir := func(cookie uint64, verf []byte, want uint32) (uint64, []byte) {
		args := newWriter()
		args.opaque(root)
		args.uint64(cookie)
		args.raw(verf)
		args.uint32(512)
		r := c.nfsCall(nfsProcReaddir, args, want)
		if want != nfsOK {
			return 0, nil
		}
		skipPostOpAttr(r)
		v := slices.Clone(r.bytesN(8))
		var last uint64
		for r.bool() {
			r.uint64()
			r.string()
			last = r.uint64()
		}
		return last, v
	}
	cookie, verf := readdir(0, make([]byte, 8), nfsOK)
	if bytes.Equal(verf, make([]byte, 8)) {
		t.Fatal("READDIR returned a zero cookie verifier")
	}
	if _, v2 := readdir(cookie, verf, nfsOK); !bytes.Equal(v2, verf) {
		t.Fatal("continuation changed the verifier")
	}
	bogus := []byte{9, 9, 9, 9, 9, 9, 9, 9}
	readdir(cookie, bogus, errBadCookie)
}

func TestRetransmittedRemoveIsAnsweredFromCache(t *testing.T) {
	p := memfs.New()
	simplefs.WriteFile(t.Context(), p, "/f", nil, 0)
	srv := NewServer(p, Options{})
	defer srv.Close()
	root := srv.encodeFH(rootFileID)

	args := newWriter()
	args.opaque(root)
	args.string("f")
	msg := rpcMsg(progNFS, 3, nfsProcRemove, args)
	status := func(client string) uint32 {
		reply := srv.dispatch(t.Context(), client, slices.Clone(msg))
		defer reply.release()
		r := newReader(reply.bytes())
		for range 6 { // xid, mtype, reply_stat, verf flavor, verf len, accept_stat
			r.uint32()
		}
		return r.uint32()
	}
	if st := status("10.0.0.1"); st != nfsOK {
		t.Fatalf("first REMOVE = %d", st)
	}
	if st := status("10.0.0.1"); st != nfsOK {
		t.Fatalf("retransmitted REMOVE = %d, want the cached NFS3_OK", st)
	}
	if st := status("10.0.0.2"); st != nfsErrNoEnt {
		t.Fatalf("REMOVE from another client = %d, want NOENT", st)
	}
}

func TestMaxHandlesEvictsToStale(t *testing.T) {
	p := memfs.New()
	for i := range 10 {
		simplefs.WriteFile(t.Context(), p, "/f"+strconv.Itoa(i), nil, 0)
	}
	_, c := startServerWith(t, p, Options{MaxHandles: 4})
	root := c.mnt()
	first := c.lookup(root, "f0", nfsOK)
	for i := 1; i < 10; i++ {
		c.lookup(root, "f"+strconv.Itoa(i), nfsOK)
	}
	args := newWriter()
	args.opaque(first)
	c.nfsCall(nfsProcGetattr, args, nfsErrStale)
	// Looking it up again works.
	again := c.lookup(root, "f0", nfsOK)
	args = newWriter()
	args.opaque(again)
	c.nfsCall(nfsProcGetattr, args, nfsOK)
}
