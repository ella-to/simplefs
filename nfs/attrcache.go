package nfs

import (
	"context"
	"sync"
	"time"

	"ella.to/simplefs"
)

// attrCache remembers recent Stat results by filehandle ID for
// Options.AttrCacheTTL, so the GETATTR/LOOKUP/ACCESS storms clients send
// (and the post-op attributes in almost every reply) do not each reach the
// provider. Every mutation the server performs drops the affected entries,
// as do WatchProvider events when the provider has them; changes made
// behind the server's back are visible after at most one TTL.
type attrCache struct {
	ttl time.Duration

	mu      sync.Mutex
	entries map[uint64]attrEntry
}

type attrEntry struct {
	info    simplefs.FileInfo
	expires time.Time
}

// maxAttrEntries bounds the cache; when it fills up it is simply reset.
const maxAttrEntries = 64 << 10

func newAttrCache(ttl time.Duration) *attrCache {
	return &attrCache{ttl: ttl, entries: map[uint64]attrEntry{}}
}

func (c *attrCache) get(id uint64) (simplefs.FileInfo, bool) {
	if c.ttl <= 0 {
		return simplefs.FileInfo{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[id]
	if !ok || time.Now().After(e.expires) {
		return simplefs.FileInfo{}, false
	}
	return e.info, true
}

func (c *attrCache) put(id uint64, info simplefs.FileInfo) {
	if c.ttl <= 0 || id == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= maxAttrEntries {
		clear(c.entries)
	}
	c.entries[id] = attrEntry{info: info, expires: time.Now().Add(c.ttl)}
}

func (c *attrCache) drop(id uint64) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	delete(c.entries, id)
	c.mu.Unlock()
}

// stat returns path's metadata, from the cache when fresh. id may be 0 when
// the caller does not know it.
func (s *export) stat(ctx context.Context, path string, id uint64) (simplefs.FileInfo, error) {
	if id == 0 {
		id, _ = s.fh.Lookup(path)
	}
	if info, ok := s.attrs.get(id); ok {
		return info, nil
	}
	info, err := s.p.Stat(ctx, path)
	if err == nil {
		s.attrs.put(id, info)
	}
	return info, err
}

// invalidate drops cached attributes (and directory listings) for paths
// the server just changed.
func (s *export) invalidate(paths ...string) {
	for _, p := range paths {
		if id, ok := s.fh.Lookup(p); ok {
			s.attrs.drop(id)
			s.dirs.drop(id)
		}
	}
}

// watchInvalidate keeps the caches honest about changes the provider makes
// itself, for providers that can report them.
func (s *export) watchInvalidate() {
	wp, ok := simplefs.As[simplefs.WatchProvider](s.p)
	if !ok {
		return
	}
	events, err := wp.Watch(s.ctx, "/", true)
	if err != nil {
		s.log.Warn("nfs: cannot watch provider; cached attributes may lag", "err", err)
		return
	}
	for ev := range events {
		s.invalidate(ev.Path, simplefs.ParentPath(ev.Path))
		if ev.OldPath != "" {
			s.invalidate(ev.OldPath, simplefs.ParentPath(ev.OldPath))
		}
	}
}
