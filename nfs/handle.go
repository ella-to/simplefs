package nfs

// NFSv3 addresses files by opaque filehandle while simplefs.FileProvider is
// path-based, so the server keeps an ID<->path table (the same idea as
// mount.Bridge), re-keying on rename and forgetting on remove. The 8-byte ID
// doubles as the fileid3 attribute, so it must stay stable for the life of a
// file. The table lives in memory: handles held by a client across a server
// restart come back NFS3ERR_STALE, which is the protocol's word for exactly
// that.

import (
	"context"
	"encoding/binary"
	"strings"
	"sync"
	"time"

	"ella.to/simplefs"
)

const rootFileID = 1

// fhTable maps 8-byte filehandle IDs to provider paths and back.
type fhTable struct {
	mu     sync.Mutex
	byID   map[uint64]string
	byPath map[string]uint64
	next   uint64
}

func newFHTable() *fhTable {
	t := &fhTable{
		byID:   map[uint64]string{rootFileID: "/"},
		byPath: map[string]uint64{"/": rootFileID},
		next:   rootFileID,
	}
	return t
}

// id returns the stable ID for path, assigning one on first sight.
func (t *fhTable) id(path string) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if id, ok := t.byPath[path]; ok {
		return id
	}
	t.next++
	t.byPath[path] = t.next
	t.byID[t.next] = path
	return t.next
}

// lookup returns the ID already assigned to path, without assigning one.
func (t *fhTable) lookup(path string) (uint64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	id, ok := t.byPath[path]
	return id, ok
}

// path resolves an ID back to a path; ok is false for stale handles.
func (t *fhTable) path(id uint64) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.byID[id]
	return p, ok
}

// rekey moves oldPath (and every mapped descendant) to newPath, keeping IDs
// stable across the rename.
func (t *fhTable) rekey(oldPath, newPath string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	prefix := oldPath + "/"
	for p, id := range t.byPath {
		var moved string
		switch {
		case p == oldPath:
			moved = newPath
		case strings.HasPrefix(p, prefix):
			moved = newPath + p[len(oldPath):]
		default:
			continue
		}
		delete(t.byPath, p)
		t.byPath[moved] = id
		t.byID[id] = moved
	}
}

// forget drops path and every mapped descendant.
func (t *fhTable) forget(path string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	prefix := path + "/"
	for p, id := range t.byPath {
		if p == path || strings.HasPrefix(p, prefix) {
			delete(t.byPath, p)
			delete(t.byID, id)
		}
	}
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
	p       simplefs.FileProvider
	mu      sync.Mutex
	entries map[uint64]*openEntry
	closed  bool
}

type openEntry struct {
	h        simplefs.FileHandle
	write    bool
	refs     int
	lastUsed time.Time
}

const openIdle = 5 * time.Second

func newOpenCache(p simplefs.FileProvider) *openCache {
	c := &openCache{p: p, entries: map[uint64]*openEntry{}}
	go c.sweep()
	return c
}

// acquire returns a live handle for the file identified by id/path, opening
// one if needed (upgrading to read-write when write is requested). Callers
// must pair it with release.
func (c *openCache) acquire(ctx context.Context, id uint64, path string, write bool) (simplefs.FileHandle, error) {
	c.mu.Lock()
	if e, ok := c.entries[id]; ok && (e.write || !write) {
		e.refs++
		e.lastUsed = time.Now()
		c.mu.Unlock()
		return e.h, nil
	}
	c.mu.Unlock()

	opts := simplefs.OpenOptions{Read: true, Write: write}
	h, err := c.p.Open(ctx, path, opts)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.entries[id]; ok {
		if old.write || !write {
			// Someone raced us to an equally capable handle; use theirs.
			go h.Close()
			old.refs++
			old.lastUsed = time.Now()
			return old.h, nil
		}
		// Ours is the more capable handle: retire the read-only one.
		if old.refs == 0 {
			go old.h.Close()
		} else {
			// Still in use; let the sweeper close it once released.
			delete(c.entries, id)
			go func(e *openEntry) {
				for {
					time.Sleep(100 * time.Millisecond)
					c.mu.Lock()
					if e.refs == 0 {
						c.mu.Unlock()
						e.h.Close()
						return
					}
					c.mu.Unlock()
				}
			}(old)
		}
	}
	e := &openEntry{h: h, write: write, refs: 1, lastUsed: time.Now()}
	c.entries[id] = e
	return h, nil
}

// release marks the handle acquired for id as no longer in use.
func (c *openCache) release(id uint64) {
	c.mu.Lock()
	if e, ok := c.entries[id]; ok && e.refs > 0 {
		e.refs--
		e.lastUsed = time.Now()
	}
	c.mu.Unlock()
}

// drop closes the cached handle for id (used when the file is removed,
// renamed over, or resized outside the handle).
func (c *openCache) drop(id uint64) {
	c.mu.Lock()
	e, ok := c.entries[id]
	if ok && e.refs == 0 {
		delete(c.entries, id)
	} else {
		ok = false
	}
	c.mu.Unlock()
	if ok {
		e.h.Close()
	}
}

// sweep closes idle unreferenced handles until the cache is shut down.
func (c *openCache) sweep() {
	for {
		time.Sleep(openIdle)
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return
		}
		var toClose []simplefs.FileHandle
		now := time.Now()
		for id, e := range c.entries {
			if e.refs == 0 && now.Sub(e.lastUsed) >= openIdle {
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

// shutdown closes every cached handle and stops the sweeper.
func (c *openCache) shutdown() {
	c.mu.Lock()
	c.closed = true
	entries := c.entries
	c.entries = map[uint64]*openEntry{}
	c.mu.Unlock()
	for _, e := range entries {
		e.h.Close()
	}
}
