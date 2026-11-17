package nfs

import (
	"testing"

	"ella.to/simplefs/memfs"
)

// rpcMsg builds an RPC call message (without TCP record marking).
func rpcMsg(prog, vers, proc uint32, args *xdrWriter) []byte {
	w := newWriter()
	w.uint32(1) // xid
	w.uint32(msgCall)
	w.uint32(2)
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
	return w.bytes()
}

// fuzzSeeds returns one well-formed call for every procedure the server
// implements, so the fuzzer starts from inputs that reach deep code paths.
func fuzzSeeds(srv *Server) [][]byte {
	root := srv.encodeFH(rootFileID)
	fhArgs := func() *xdrWriter { w := newWriter(); w.opaque(root); return w }
	dirop := func(name string) *xdrWriter { w := fhArgs(); w.string(name); return w }
	sattr := func(w *xdrWriter) *xdrWriter {
		w.bool(true)
		w.uint32(0o644)
		w.bool(false)
		w.bool(false)
		w.bool(true)
		w.uint64(10)
		w.uint32(1)
		w.uint32(2)
		w.uint32(1)
		w.uint32(2)
		return w
	}

	var seeds [][]byte
	add := func(prog, vers, proc uint32, a *xdrWriter) { seeds = append(seeds, rpcMsg(prog, vers, proc, a)) }

	mnt := newWriter()
	mnt.string("/")
	add(progMount, 3, mountProcMnt, mnt)
	add(progMount, 3, mountProcExport, nil)
	gp := newWriter()
	gp.uint32(progNFS)
	gp.uint32(3)
	gp.uint32(protoTCP)
	gp.uint32(0)
	add(progPortmap, 2, pmapProcGetport, gp)
	add(progPortmap, 2, pmapProcDump, nil)
	lock := newWriter()
	lock.opaque([]byte("cookie"))
	add(progNLM, 4, nlmProcLock, lock)

	add(progNFS, 3, nfsProcNull, nil)
	add(progNFS, 3, nfsProcGetattr, fhArgs())
	sa := sattr(fhArgs())
	sa.bool(false)
	add(progNFS, 3, nfsProcSetattr, sa)
	add(progNFS, 3, nfsProcLookup, dirop("a"))
	acc := fhArgs()
	acc.uint32(0x3f)
	add(progNFS, 3, nfsProcAccess, acc)
	add(progNFS, 3, nfsProcReadlink, fhArgs())
	rd := fhArgs()
	rd.uint64(0)
	rd.uint32(4096)
	add(progNFS, 3, nfsProcRead, rd)
	wr := fhArgs()
	wr.uint64(0)
	wr.uint32(5)
	wr.uint32(writeFileSync)
	wr.opaque([]byte("hello"))
	add(progNFS, 3, nfsProcWrite, wr)
	cr := dirop("f")
	cr.uint32(createGuarded)
	add(progNFS, 3, nfsProcCreate, sattr(cr))
	ex := dirop("x")
	ex.uint32(createExclusive)
	ex.raw(make([]byte, 8))
	add(progNFS, 3, nfsProcCreate, ex)
	add(progNFS, 3, nfsProcMkdir, sattr(dirop("d")))
	sl := sattr(dirop("l"))
	sl.string("target")
	add(progNFS, 3, nfsProcSymlink, sl)
	add(progNFS, 3, nfsProcRemove, dirop("f"))
	add(progNFS, 3, nfsProcRmdir, dirop("d"))
	rn := dirop("a")
	rn.opaque(root)
	rn.string("b")
	add(progNFS, 3, nfsProcRename, rn)
	ln := fhArgs()
	ln.opaque(root)
	ln.string("h")
	add(progNFS, 3, nfsProcLink, ln)
	rdd := fhArgs()
	rdd.uint64(0)
	rdd.raw(make([]byte, 8))
	rdd.uint32(4096)
	add(progNFS, 3, nfsProcReaddir, rdd)
	rdp := fhArgs()
	rdp.uint64(0)
	rdp.raw(make([]byte, 8))
	rdp.uint32(4096)
	rdp.uint32(8192)
	add(progNFS, 3, nfsProcReaddirplus, rdp)
	add(progNFS, 3, nfsProcFsstat, fhArgs())
	add(progNFS, 3, nfsProcFsinfo, fhArgs())
	add(progNFS, 3, nfsProcPathconf, fhArgs())
	cm := fhArgs()
	cm.uint64(0)
	cm.uint32(0)
	add(progNFS, 3, nfsProcCommit, cm)
	return seeds
}

func FuzzDecodeCall(f *testing.F) {
	for _, s := range fuzzSeeds(NewServer(memfs.New(), Options{})) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, msg []byte) {
		c, err := decodeCall(msg)
		if err == nil && c != nil && c.args == nil {
			t.Fatal("decoded call without an args reader")
		}
	})
}

func FuzzDispatch(f *testing.F) {
	srv := NewServer(memfs.New(memfs.WithCapacity(1<<20)), Options{})
	// A fixed boot verifier keeps the corpus' handles valid across runs.
	srv.bootID = [8]byte{}
	for _, s := range fuzzSeeds(srv) {
		f.Add(s)
	}
	f.Cleanup(func() { srv.Close() })
	f.Fuzz(func(t *testing.T, msg []byte) {
		srv.dispatch(t.Context(), msg) // must never panic, whatever the bytes
	})
}
