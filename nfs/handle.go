package nfs

// NFSv3 addresses files by opaque filehandle while simplefs.FileProvider is
// path-based, so the server keeps an ID<->path table (internal/idtable, as
// fileprovider.Bridge does), re-keying on rename and forgetting on remove. The 8-byte ID
// doubles as the fileid3 attribute, so it must stay stable for the life of a
// file. The table lives in memory: handles held by a client across a server
// restart come back NFS3ERR_STALE, which is the protocol's word for exactly
// that.

import (
	"context"
	"encoding/binary"
	"sync"
	"time"

	"ella.to/simplefs"
	"ella.to/simplefs/internal/idtable"
)

const rootFileID = 1

// newFHTable returns the filehandle table: "/" is rootFileID, and new paths
// get increasing IDs (called under the table's lock).
func newFHTable() *idtable.Table[uint64] {
	next := uint64(rootFileID)
	return idtable.New(uint64(rootFileID), func() uint64 { next++; return next })
}

// encodeFH renders an ID as the wire filehandle (opaque, 8 bytes).
func encodeFH(id uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], id)
	return b[:]
}

// decodeFH parses a wire filehandle produced by encodeFH.
func decodeFH(b []byte) (uint64, bool) {
	if len(b) != 8 {
		return 0, false
	}
	return binary.BigEndian.Uint64(b), true
}

// openCache keeps provider FileHandles alive across the many independent
// READ/WRITE calls NFS issues for one logical file access. NFSv3 is
// stateless — there is no open/close on the wire — so without a cache every
// 128 KiB chunk would pay a full provider Open/Close round trip. Entries are
// closed after sitting idle, or eagerly when the path is removed, renamed,
// or truncated.
type openCache struct {
	p    simplefs.FileProvider
	idle time.Duration

	mu      sync.Mutex
	entries map[uint64]*openEntry
	closed  bool

	stop chan struct{}
	done chan struct{}
}

type openEntry struct {
	h        simplefs.FileHandle
	write    bool
	refs     int
	lastUsed time.Time

	// retired entries are no longer in the cache map (superseded by a
	// read-write handle, or their file was removed) and are closed by the
	// last release.
	retired bool
}

const defaultHandleIdle = 5 * time.Second

func newOpenCache(p simplefs.FileProvider, idle time.Duration) *openCache {
	if idle <= 0 {
		idle = defaultHandleIdle
	}
	c := &openCache{
		p:       p,
		idle:    idle,
		entries: map[uint64]*openEntry{},
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go c.sweep()
	return c
}

// acquire returns a live handle for the file identified by id/path, opening
// one if needed (upgrading to read-write when write is requested). Callers
// must call the returned release func exactly once when done with h.
func (c *openCache) acquire(ctx context.Context, id uint64, path string, write bool) (h simplefs.FileHandle, release func(), err error) {
	c.mu.Lock()
	if e, ok := c.entries[id]; ok && (e.write || !write) {
		e.refs++
		e.lastUsed = time.Now()
		c.mu.Unlock()
		return e.h, func() { c.release(e) }, nil
	}
	c.mu.Unlock()

	h, err = c.p.Open(ctx, path, simplefs.OpenOptions{Read: true, Write: write})
	if err != nil {
		return nil, nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.entries[id]; ok {
		if old.write || !write {
			// Someone raced us to an equally capable handle; use theirs.
			go h.Close()
			old.refs++
			old.lastUsed = time.Now()
			return old.h, func() { c.release(old) }, nil
		}
		// Ours is the more capable handle: retire the read-only one.
		c.retireLocked(id, old)
	}
	e := &openEntry{h: h, write: write, refs: 1, lastUsed: time.Now()}
	if c.closed {
		e.retired = true // closed by release; never cached
	} else {
		c.entries[id] = e
	}
	return h, func() { c.release(e) }, nil
}

// retireLocked removes e from the map and closes it now if unused, or marks
// it so the last release closes it.
func (c *openCache) retireLocked(id uint64, e *openEntry) {
	if c.entries[id] == e {
		delete(c.entries, id)
	}
	e.retired = true
	if e.refs == 0 {
		go e.h.Close()
	}
}

// release marks one use of e as finished.
func (c *openCache) release(e *openEntry) {
	c.mu.Lock()
	e.refs--
	e.lastUsed = time.Now()
	closeNow := e.retired && e.refs == 0
	c.mu.Unlock()
	if closeNow {
		e.h.Close()
	}
}

// drop evicts the cached handle for id (used when the file is removed,
// renamed over, or resized outside the handle). A handle still in use is
// closed when its last user releases it.
func (c *openCache) drop(id uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[id]; ok {
		c.retireLocked(id, e)
	}
}

// sweep closes idle unreferenced handles until the cache is shut down.
func (c *openCache) sweep() {
	defer close(c.done)
	t := time.NewTicker(c.idle / 2)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case now := <-t.C:
			var toClose []simplefs.FileHandle
			c.mu.Lock()
			for id, e := range c.entries {
				if e.refs == 0 && now.Sub(e.lastUsed) >= c.idle {
					toClose = append(toClose, e.h)
					delete(c.entries, id)
				}
			}
			c.mu.Unlock()
			for _, h := range toClose {
				h.Close()
			}
		}
	}
}

// shutdown stops the sweeper and closes every cached handle. Handles still
// in use are closed by their last release.
func (c *openCache) shutdown() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	entries := c.entries
	c.entries = map[uint64]*openEntry{}
	var toClose []simplefs.FileHandle
	for _, e := range entries {
		e.retired = true
		if e.refs == 0 {
			toClose = append(toClose, e.h)
		}
	}
	c.mu.Unlock()
	close(c.stop)
	<-c.done
	for _, h := range toClose {
		h.Close()
	}
}
