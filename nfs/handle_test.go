package nfs

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"ella.to/simplefs"
	"ella.to/simplefs/memfs"
)

// closeCounter counts handles closed through it.
type closeCounter struct {
	*memfs.MemFS
	opened, closed atomic.Int32
}

type countedHandle struct {
	simplefs.FileHandle
	c *closeCounter
}

func (h countedHandle) Close() error {
	h.c.closed.Add(1)
	return h.FileHandle.Close()
}

func (c *closeCounter) Open(ctx context.Context, path string, opts simplefs.OpenOptions) (simplefs.FileHandle, error) {
	h, err := c.MemFS.Open(ctx, path, opts)
	if err != nil {
		return nil, err
	}
	c.opened.Add(1)
	return countedHandle{h, c}, nil
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestOpenCacheUpgradeRetiresBusyHandle(t *testing.T) {
	p := &closeCounter{MemFS: memfs.New()}
	h, _ := p.MemFS.Create(t.Context(), "/f", simplefs.CreateOptions{})
	h.Close()

	c := newOpenCache(p, time.Hour)
	defer c.shutdown()

	_, relRO, err := c.acquire(t.Context(), 7, "/f", false)
	if err != nil {
		t.Fatal(err)
	}
	_, relRW, err := c.acquire(t.Context(), 7, "/f", true)
	if err != nil {
		t.Fatal(err)
	}
	if n := p.closed.Load(); n != 0 {
		t.Fatalf("busy read-only handle closed early (%d closes)", n)
	}
	relRO() // last user of the retired handle closes it
	waitFor(t, func() bool { return p.closed.Load() == 1 })

	relRW()
	c.drop(7) // unused -> closed right away
	waitFor(t, func() bool { return p.closed.Load() == 2 })
}

func TestOpenCacheDropBusyClosesOnRelease(t *testing.T) {
	p := &closeCounter{MemFS: memfs.New()}
	h, _ := p.MemFS.Create(t.Context(), "/f", simplefs.CreateOptions{})
	h.Close()

	c := newOpenCache(p, time.Hour)
	defer c.shutdown()
	_, rel, err := c.acquire(t.Context(), 1, "/f", true)
	if err != nil {
		t.Fatal(err)
	}
	c.drop(1)
	if p.closed.Load() != 0 {
		t.Fatal("dropped handle closed while in use")
	}
	rel()
	waitFor(t, func() bool { return p.closed.Load() == 1 })
}

func TestOpenCacheSweepsIdle(t *testing.T) {
	p := &closeCounter{MemFS: memfs.New()}
	h, _ := p.MemFS.Create(t.Context(), "/f", simplefs.CreateOptions{})
	h.Close()

	c := newOpenCache(p, 20*time.Millisecond)
	defer c.shutdown()
	_, rel, err := c.acquire(t.Context(), 1, "/f", false)
	if err != nil {
		t.Fatal(err)
	}
	rel()
	waitFor(t, func() bool { return p.closed.Load() == 1 })
}
