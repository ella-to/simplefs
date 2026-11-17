package fileprovider

import (
	"context"
	"errors"
	"sync"
	"time"

	"ella.to/simplefs"
)

// CodeExpired reports that a sync anchor is too old to answer from the
// change log; the system then re-enumerates everything.
const CodeExpired Code = 8

// changeLog records which items changed after each sync anchor, so the
// system learns about changes it did not make itself (another process
// writing to the backend, a sync from elsewhere).
type changeLog struct {
	mu      sync.Mutex
	anchor  uint64   // current anchor: the seq of the newest change
	floor   uint64   // anchors below this can no longer be answered
	entries []change // oldest first, bounded by maxChanges
}

type change struct {
	seq     uint64
	id      string
	deleted bool
}

const (
	maxChanges   = 10_000
	pollInterval = 30 * time.Second
	notifyDelay  = 200 * time.Millisecond
)

func (c *changeLog) add(id string, deleted bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.anchor++
	c.entries = append(c.entries, change{seq: c.anchor, id: id, deleted: deleted})
	if len(c.entries) > maxChanges {
		drop := len(c.entries) - maxChanges
		c.floor = c.entries[drop-1].seq
		c.entries = append(c.entries[:0], c.entries[drop:]...)
	}
}

// expireAll moves the anchor so every earlier anchor is expired: the
// system must re-enumerate (used when changes cannot be observed).
func (c *changeLog) expireAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.anchor++
	c.floor = c.anchor
	c.entries = c.entries[:0]
}

// Anchor returns the current sync anchor.
func (b *Bridge) Anchor() uint64 {
	b.changes.mu.Lock()
	defer b.changes.mu.Unlock()
	return b.changes.anchor
}

// ChangesSince returns the items updated and the IDs deleted after anchor,
// and the anchor to resume from. An anchor the log no longer covers fails
// with CodeExpired.
func (b *Bridge) ChangesSince(anchor uint64) (updated []Item, deleted []string, next uint64, err error) {
	c := &b.changes
	c.mu.Lock()
	if anchor < c.floor || anchor > c.anchor {
		c.mu.Unlock()
		return nil, nil, 0, &Error{Code: CodeExpired, Msg: "sync anchor expired"}
	}
	last := map[string]bool{} // id -> deleted, last change wins
	var order []string
	for _, ch := range c.entries {
		if ch.seq <= anchor {
			continue
		}
		if _, seen := last[ch.id]; !seen {
			order = append(order, ch.id)
		}
		last[ch.id] = ch.deleted
	}
	next = c.anchor
	c.mu.Unlock()

	for _, id := range order {
		if last[id] {
			deleted = append(deleted, id)
			continue
		}
		it, err := b.Stat(id)
		switch {
		case err == nil:
			updated = append(updated, it)
		case errors.Is(err, simplefs.ErrNotExist) || isCode(err, CodeNotFound):
			deleted = append(deleted, id)
		default:
			return nil, nil, 0, err
		}
	}
	return updated, deleted, next, nil
}

func isCode(err error, code Code) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// TrackChanges feeds the change log until ctx ends and calls notify
// (debounced) whenever there is something new for the system to fetch.
// With a WatchProvider every event is recorded; otherwise the anchor is
// expired every 30s so the system re-enumerates.
func (b *Bridge) TrackChanges(ctx context.Context, notify func()) {
	kick := make(chan struct{}, 1)
	signal := func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-kick:
				time.Sleep(notifyDelay) // coalesce bursts into one signal
				select {
				case <-kick:
				default:
				}
				notify()
			}
		}
	}()

	wp, ok := simplefs.As[simplefs.WatchProvider](b.p)
	var events <-chan simplefs.Event
	if ok {
		var err error
		if events, err = wp.Watch(ctx, "/", true); err != nil {
			ok = false
		}
	}
	if !ok {
		t := time.NewTicker(pollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				b.changes.expireAll()
				signal()
			}
		}
	}
	for ev := range events {
		b.recordEvent(ev)
		signal()
	}
}

// recordEvent turns a provider event into change-log entries, keeping the
// ID table in step with renames and deletions made outside the bridge.
func (b *Bridge) recordEvent(ev simplefs.Event) {
	switch ev.Type {
	case simplefs.EventDelete:
		if id, ok := b.ids.Lookup(ev.Path); ok {
			b.ids.Forget(ev.Path)
			b.logForget(ev.Path)
			b.changes.add(id, true)
		}
	case simplefs.EventRename:
		if _, ok := b.ids.Lookup(ev.OldPath); ok {
			b.ids.Rekey(ev.OldPath, ev.Path)
			b.logRekey(ev.OldPath, ev.Path)
		}
		b.changes.add(b.idFor(ev.Path), false)
	default: // create, modify
		if ev.Path != "/" {
			b.changes.add(b.idFor(ev.Path), false)
		}
	}
}
