package dirfs

import (
	"sync"
	"time"
)

// cursorCache remembers the rest of a directory listing after each page, so
// paging through a large directory costs O(page) per call instead of
// re-reading and re-sorting the whole directory every time. Entries expire,
// are dropped when this provider mutates the directory, and are bounded.
type cursorCache struct {
	mu      sync.Mutex
	entries map[cursorKey]*cursor
}

type cursorKey struct{ dir, token string }

type cursor struct {
	names []string
	at    time.Time
}

const (
	cursorTTL = 30 * time.Second
	maxCursor = 64
)

func newCursorCache() *cursorCache {
	return &cursorCache{entries: map[cursorKey]*cursor{}}
}

// take returns and forgets the names that follow token in dir.
func (c *cursorCache) take(dir, token string) ([]string, bool) {
	if token == "" {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	k := cursorKey{dir, token}
	e, ok := c.entries[k]
	if !ok {
		return nil, false
	}
	delete(c.entries, k)
	if time.Since(e.at) > cursorTTL {
		return nil, false
	}
	return e.names, true
}

// put remembers names as the continuation after token in dir.
func (c *cursorCache) put(dir, token string, names []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= maxCursor {
		var oldest cursorKey
		var oldestAt time.Time
		for k, e := range c.entries {
			if oldestAt.IsZero() || e.at.Before(oldestAt) {
				oldest, oldestAt = k, e.at
			}
		}
		delete(c.entries, oldest)
	}
	c.entries[cursorKey{dir, token}] = &cursor{names: names, at: time.Now()}
}

// invalidate drops every cursor of dir.
func (c *cursorCache) invalidate(dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if k.dir == dir {
			delete(c.entries, k)
		}
	}
}
