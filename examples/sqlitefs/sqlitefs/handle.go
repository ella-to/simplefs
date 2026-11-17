package sqlitefs

import (
	"context"
	"io"
	"math"
	"sync"
	"time"

	"ella.to/simplefs"
	"ella.to/sqlite"
)

// handle is an open file. It holds the node id, not the path, so it keeps
// working when the file (or any directory above it) is renamed, and Stat
// recomputes the current path from the parent chain.
type handle struct {
	f    *FS
	id   int64
	opts simplefs.OpenOptions

	mu     sync.Mutex
	closed bool
	path   string // last known path, reported once the file is unlinked
}

var _ simplefs.FileHandle = (*handle)(nil)

// check fails if the handle is closed or lacks the access write needs.
func (h *handle) check(op string, write bool) error {
	h.mu.Lock()
	closed, path := h.closed, h.path
	h.mu.Unlock()
	switch {
	case closed:
		return patherr(op, path, simplefs.ErrClosed)
	case write && !h.opts.Write:
		return patherr(op, path, simplefs.ErrPermission)
	case !write && !h.opts.Read:
		return patherr(op, path, simplefs.ErrPermission)
	}
	return nil
}

func (h *handle) lastPath() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.path
}

// ReadAt reads from the chunks overlapping [off, off+len(p)) in one read
// transaction. Missing chunks, and the part of the file past a chunk's
// stored length, read as zeros.
func (h *handle) ReadAt(p []byte, off int64) (int, error) {
	if err := h.check("read", false); err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, patherr("read", h.lastPath(), simplefs.ErrInvalid)
	}
	ctx := context.Background()
	var n int
	err := h.f.read(ctx, func(conn *sqlite.Conn) error {
		node, err := getNode(ctx, conn, h.id)
		if err != nil {
			return err
		}
		if off >= node.size {
			return nil
		}
		n = int(min(int64(len(p)), node.size-off))
		buf := p[:n]
		clear(buf)
		end := off + int64(n)
		return query(ctx, conn, func(s *sqlite.Stmt) error {
			start := s.ColumnInt64(0) * ChunkSize
			if start >= off {
				// The chunk begins inside the range: copy straight in.
				s.ColumnBytes(1, buf[start-off:])
				return nil
			}
			// Only the first chunk can begin before off.
			data := make([]byte, s.ColumnLen(1))
			s.ColumnBytes(1, data)
			if skip := off - start; skip < int64(len(data)) {
				copy(buf, data[skip:])
			}
			return nil
		}, "SELECT idx, data FROM chunks WHERE node_id = ? AND idx BETWEEN ? AND ? ORDER BY idx",
			h.id, off/ChunkSize, (end-1)/ChunkSize)
	})
	if err != nil {
		return 0, patherr("read", h.lastPath(), err)
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// WriteAt writes p at off in one write transaction. A chunk p covers
// completely is replaced without being read; a partly covered one is read,
// patched and written back. A write past the end leaves a gap that simply
// has no chunks.
func (h *handle) WriteAt(p []byte, off int64) (int, error) {
	if err := h.check("write", true); err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, patherr("write", h.lastPath(), simplefs.ErrInvalid)
	}
	ctx := context.Background()
	err := h.f.write(ctx, func(conn *sqlite.Conn) error {
		node, err := getNode(ctx, conn, h.id)
		if err != nil {
			return err
		}
		if h.opts.Append {
			off = node.size
		}
		if off > math.MaxInt64-int64(len(p)) {
			return simplefs.ErrTooLarge
		}
		if len(p) == 0 {
			return nil
		}
		size := max(node.size, off+int64(len(p)))
		if err := exec(ctx, conn, "UPDATE nodes SET size = ?, mtime = ?, version = version + 1 WHERE id = ?",
			size, time.Now().UnixNano(), h.id); err != nil {
			return err
		}
		for rest, at := p, off; len(rest) > 0; {
			idx, within := at/ChunkSize, int(at%ChunkSize)
			k := min(len(rest), ChunkSize-within)
			data := rest[:k]
			if k < ChunkSize {
				var old []byte
				if _, err := queryRow(ctx, conn, func(s *sqlite.Stmt) {
					old = make([]byte, s.ColumnLen(0))
					s.ColumnBytes(0, old)
				}, "SELECT data FROM chunks WHERE node_id = ? AND idx = ?", h.id, idx); err != nil {
					return err
				}
				data = make([]byte, max(len(old), within+k))
				copy(data, old)
				copy(data[within:], rest[:k])
			}
			if err := exec(ctx, conn,
				`INSERT INTO chunks (node_id, idx, data) VALUES (?, ?, ?)
				ON CONFLICT (node_id, idx) DO UPDATE SET data = excluded.data`,
				h.id, idx, data); err != nil {
				return err
			}
			rest, at = rest[k:], at+int64(k)
		}
		return nil
	})
	if err != nil {
		return 0, patherr("write", h.lastPath(), err)
	}
	return len(p), nil
}

// Truncate changes the size. Shrinking deletes the chunks wholly past the
// new end and trims the one it falls in (substr on a BLOB counts bytes), so
// no stale bytes remain to reappear if the file grows again. Growing only
// updates the size: the new tail has no chunks and reads as zeros.
func (h *handle) Truncate(ctx context.Context, size int64) error {
	if err := h.check("truncate", true); err != nil {
		return err
	}
	if size < 0 {
		return patherr("truncate", h.lastPath(), simplefs.ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	err := h.f.write(ctx, func(conn *sqlite.Conn) error {
		node, err := getNode(ctx, conn, h.id)
		if err != nil {
			return err
		}
		if size == node.size {
			return nil
		}
		if err := exec(ctx, conn, "UPDATE nodes SET size = ?, mtime = ?, version = version + 1 WHERE id = ?",
			size, time.Now().UnixNano(), h.id); err != nil {
			return err
		}
		if size > node.size {
			return nil
		}
		if size == 0 {
			return exec(ctx, conn, "DELETE FROM chunks WHERE node_id = ?", h.id)
		}
		last := (size - 1) / ChunkSize
		if err := exec(ctx, conn, "DELETE FROM chunks WHERE node_id = ? AND idx > ?", h.id, last); err != nil {
			return err
		}
		keep := size - last*ChunkSize
		return exec(ctx, conn, "UPDATE chunks SET data = substr(data, 1, ?) WHERE node_id = ? AND idx = ? AND length(data) > ?",
			keep, h.id, last, keep)
	})
	if err != nil {
		return patherr("truncate", h.lastPath(), err)
	}
	return nil
}

// Stat returns the file's metadata and its current path, computed from the
// parent chain. A file unlinked while open reports its last path.
func (h *handle) Stat(ctx context.Context) (simplefs.FileInfo, error) {
	if h.isClosed() {
		return simplefs.FileInfo{}, patherr("stat", h.lastPath(), simplefs.ErrClosed)
	}
	if err := ctx.Err(); err != nil {
		return simplefs.FileInfo{}, err
	}
	var (
		n    node
		path string
		ok   bool
	)
	err := h.f.read(ctx, func(conn *sqlite.Conn) (err error) {
		if n, err = getNode(ctx, conn, h.id); err != nil {
			return err
		}
		path, ok, err = nodePath(ctx, conn, h.id)
		return err
	})
	if err != nil {
		return simplefs.FileInfo{}, patherr("stat", h.lastPath(), err)
	}
	h.mu.Lock()
	if ok {
		h.path = path
	} else {
		path = h.path
	}
	h.mu.Unlock()
	return n.info(path), nil
}

func (h *handle) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// Sync has nothing to do: every WriteAt and Truncate was its own committed
// transaction, made durable by synchronous=FULL (see PrepareConn).
func (h *handle) Sync(ctx context.Context) error {
	if h.isClosed() {
		return patherr("sync", h.lastPath(), simplefs.ErrClosed)
	}
	return ctx.Err()
}

// Close releases the handle. If the file was removed while open, the last
// Close deletes it.
func (h *handle) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return patherr("close", h.path, simplefs.ErrClosed)
	}
	h.closed = true
	path := h.path
	h.mu.Unlock()
	if err := h.f.release(context.Background(), h.id); err != nil {
		return patherr("close", path, err)
	}
	return nil
}
