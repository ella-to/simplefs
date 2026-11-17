package nfs

import (
	"hash/crc32"
	"net"
	"sync"
)

// drc is a duplicate request cache. NFS clients retransmit calls whose
// reply was lost or slow; replaying a REMOVE or RENAME that already
// succeeded would report a spurious error (NOENT) to the application. For
// non-idempotent procedures the server therefore remembers recent replies
// and answers a retransmission with the original reply. A retransmission of
// a call still being processed is dropped (the client will retry again).
type drc struct {
	mu      sync.Mutex
	entries map[drcKey]*drcEntry
	ring    []drcKey // insertion order, for eviction
	next    int
}

// drcKey identifies a call: the client's IP (not port: clients reconnect
// from new ports), its xid, the procedure, and a checksum of the arguments
// so different clients behind one NAT address cannot collide on xid alone.
type drcKey struct {
	client string
	xid    uint32
	proc   uint32
	sum    uint32
}

type drcEntry struct {
	done  bool
	reply []byte // payload of the reply; nil when none was sent
}

const drcSize = 1024

func newDRC() *drc {
	return &drc{entries: make(map[drcKey]*drcEntry, drcSize), ring: make([]drcKey, drcSize)}
}

// nonIdempotent reports whether an NFSv3 procedure must not be re-executed
// on retransmission.
func nonIdempotent(proc uint32) bool {
	switch proc {
	case nfsProcSetattr, nfsProcCreate, nfsProcMkdir, nfsProcSymlink, nfsProcMknod,
		nfsProcRemove, nfsProcRmdir, nfsProcRename, nfsProcLink:
		return true
	}
	return false
}

func newDRCKey(client string, c *rpcCall) drcKey {
	return drcKey{client: client, xid: c.xid, proc: c.proc, sum: crc32.ChecksumIEEE(c.args.b[c.args.off:])}
}

// begin registers a call. If it is a retransmission, dup is true and reply
// is the cached reply payload (nil: drop it, the original is in progress or
// sent nothing).
func (d *drc) begin(k drcKey) (reply []byte, dup bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if e, ok := d.entries[k]; ok {
		if e.done {
			return e.reply, true
		}
		return nil, true
	}
	if old := d.ring[d.next]; old != (drcKey{}) {
		delete(d.entries, old)
	}
	d.ring[d.next] = k
	d.next = (d.next + 1) % drcSize
	d.entries[k] = &drcEntry{}
	return nil, false
}

// finish records the reply payload sent for k.
func (d *drc) finish(k drcKey, payload []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if e, ok := d.entries[k]; ok {
		e.done = true
		e.reply = append([]byte(nil), payload...)
	}
}

// clientIP extracts the IP part of a remote address.
func clientIP(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}
