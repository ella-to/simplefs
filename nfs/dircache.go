package nfs

import (
	"context"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"
)

// dirCache keeps the listing a READDIR(PLUS) sequence is paging through.
// The first call (cookie 0) lists the directory once; continuation calls
// index into that snapshot instead of re-listing everything, which made
// listing an n-entry directory O(n²) provider work. A snapshot is
// identified by its cookie verifier, dropped when the server changes the
// directory, and expires when idle.
type dirCache struct {
	mu    sync.Mutex
	snaps map[uint64]*dirSnap // by directory filehandle ID
	total int                 // entries across all snapshots
	seq   atomic.Uint64       // source of verifiers
}

type dirSnap struct {
	verf     [8]byte
	entries  []dirEntry
	lastUsed time.Time
}

const (
	dirSnapIdle     = 30 * time.Second
	maxDirSnaps     = 64
	maxSnapEntries  = 2 << 20 // across all snapshots
	errBadCookie    = 10003   // NFS3ERR_BAD_COOKIE
	cookieVerfBytes = 8
)

func newDirCache() *dirCache { return &dirCache{snaps: map[uint64]*dirSnap{}} }

// get returns the snapshot of dir matching verf, if still cached.
func (c *dirCache) get(dir uint64, verf [8]byte) (*dirSnap, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sn, ok := c.snaps[dir]
	// A zero verifier is accepted for the live snapshot: some clients never
	// echo the verifier back.
	if !ok || (sn.verf != verf && verf != [8]byte{}) || time.Since(sn.lastUsed) > dirSnapIdle {
		return nil, false
	}
	sn.lastUsed = time.Now()
	return sn, true
}

// put stores a fresh snapshot of dir and returns it.
func (c *dirCache) put(dir uint64, entries []dirEntry) *dirSnap {
	sn := &dirSnap{entries: entries, lastUsed: time.Now()}
	binary.BigEndian.PutUint64(sn.verf[:], c.seq.Add(1))
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeLocked(dir)
	c.snaps[dir] = sn
	c.total += len(entries)
	for (len(c.snaps) > maxDirSnaps || c.total > maxSnapEntries) && len(c.snaps) > 1 {
		var oldest uint64
		var at time.Time
		for id, s := range c.snaps {
			if id != dir && (at.IsZero() || s.lastUsed.Before(at)) {
				oldest, at = id, s.lastUsed
			}
		}
		c.removeLocked(oldest)
	}
	return sn
}

func (c *dirCache) removeLocked(dir uint64) {
	if old, ok := c.snaps[dir]; ok {
		c.total -= len(old.entries)
		delete(c.snaps, dir)
	}
}

// drop forgets dir's snapshot (the directory changed).
func (c *dirCache) drop(dir uint64) {
	c.mu.Lock()
	c.removeLocked(dir)
	c.mu.Unlock()
}

// snapshot returns the listing to serve a READDIR at cookie: a fresh one
// for cookie 0, the cached one for a continuation. A continuation whose
// snapshot is gone (expired, or the directory changed) is answered from a
// fresh listing at the same index — the best a stateless protocol can do —
// while one carrying a different verifier than the live snapshot gets
// NFS3ERR_BAD_COOKIE so the client restarts.
func (s *export) snapshot(ctx context.Context, dir string, dirID, cookie uint64, verf [8]byte) (*dirSnap, uint32) {
	if cookie != 0 {
		if sn, ok := s.dirs.get(dirID, verf); ok {
			return sn, nfsOK
		}
		if verf != ([8]byte{}) && s.dirs.hasOther(dirID, verf) {
			return nil, errBadCookie
		}
	}
	entries, err := s.listDir(ctx, dir, dirID)
	if err != nil {
		return nil, s.errStatus(err)
	}
	return s.dirs.put(dirID, entries), nfsOK
}

// hasOther reports whether dir has a live snapshot with another verifier.
func (c *dirCache) hasOther(dir uint64, verf [8]byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	sn, ok := c.snaps[dir]
	return ok && sn.verf != verf && time.Since(sn.lastUsed) <= dirSnapIdle
}
