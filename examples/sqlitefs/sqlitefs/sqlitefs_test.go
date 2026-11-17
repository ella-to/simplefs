package sqlitefs_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"

	"ella.to/simplefs"
	"ella.to/simplefs/examples/sqlitefs/sqlitefs"
	"ella.to/simplefs/providertest"
	"ella.to/sqlite"
)

// openFile opens a fresh file-backed FS that is closed with the test.
func openFile(t *testing.T, path string) *sqlitefs.FS {
	t.Helper()
	f, err := sqlitefs.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

var memSeq atomic.Int64

// openMemory opens a fresh in-memory FS. sqlite.WithMemory always names the
// same database ("file::memory:"), so every caller in a process would
// share it; a unique name on the memdb VFS gives each test its own
// database, still shared by all connections of the pool.
func openMemory(t *testing.T) *sqlitefs.FS {
	t.Helper()
	ctx := t.Context()
	uri := fmt.Sprintf("file:/sqlitefs-test-%d?vfs=memdb", memSeq.Add(1))
	db, err := sqlite.New(ctx,
		sqlite.WithStringConn(uri),
		sqlite.WithPoolSize(4),
		sqlite.WithConnPrepareFunc(sqlitefs.PrepareConn),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	f, err := sqlitefs.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestConformanceFile(t *testing.T) {
	providertest.Run(t, func(t *testing.T) simplefs.FileProvider {
		return openFile(t, filepath.Join(t.TempDir(), "fs.db"))
	}, providertest.Options{})
}

func TestConformanceMemory(t *testing.T) {
	providertest.Run(t, func(t *testing.T) simplefs.FileProvider {
		return openMemory(t)
	}, providertest.Options{})
}

// TestPersistence writes a tree, closes the database, reopens it, and
// expects the same tree, content, metadata and ids.
func TestPersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fs.db")

	f, err := sqlitefs.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	must(t, simplefs.MkdirAll(ctx, f, "/a/b/c", 0o700))
	must(t, simplefs.WriteFile(ctx, f, "/a/b/c/small.txt", []byte("hello"), 0o600))
	big := pattern(3*sqlitefs.ChunkSize + 123)
	must(t, simplefs.WriteFile(ctx, f, "/a/big.bin", big, 0))
	must(t, f.Symlink(ctx, "b/c/small.txt", "/a/link"))
	before := snapshot(t, f)
	must(t, f.Close())

	f, err = sqlitefs.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	after := snapshot(t, f)
	if len(before) != len(after) {
		t.Fatalf("reopened tree has %d entries, want %d", len(after), len(before))
	}
	for p, want := range before {
		if got := after[p]; got != want {
			t.Errorf("%s after reopen = %+v, want %+v", p, got, want)
		}
	}
	data, err := simplefs.ReadFile(ctx, f, "/a/big.bin")
	must(t, err)
	if !bytes.Equal(data, big) {
		t.Error("big.bin content changed across reopen")
	}
	if target, err := f.Readlink(ctx, "/a/link"); err != nil || target != "b/c/small.txt" {
		t.Errorf("Readlink after reopen = %q, %v", target, err)
	}
}

// entry is the comparable part of a FileInfo plus file content.
type entry struct {
	typ     simplefs.FileType
	size    int64
	mode    simplefs.FileMode
	mtime   int64
	etag    string
	id      uint64
	content string
}

func snapshot(t *testing.T, f *sqlitefs.FS) map[string]entry {
	t.Helper()
	ctx := context.Background()
	out := map[string]entry{}
	err := simplefs.WalkDir(ctx, f, "/", func(p string, d simplefs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := f.Stat(ctx, p)
		if err != nil {
			return err
		}
		e := entry{info.Type, info.Size, info.Mode, info.ModTime.UnixNano(), info.ETag, info.FileID, ""}
		if info.Type.IsFile() {
			data, err := simplefs.ReadFile(ctx, f, p)
			if err != nil {
				return err
			}
			e.content = string(data)
		}
		out[p] = e
		return nil
	})
	must(t, err)
	return out
}

// TestChunks exercises writes, reads and truncates around chunk boundaries.
func TestChunks(t *testing.T) {
	const cs = sqlitefs.ChunkSize
	ctx := context.Background()
	f := openFile(t, filepath.Join(t.TempDir(), "fs.db"))
	h, err := f.Open(ctx, "/f", simplefs.OpenOptions{Read: true, Write: true, Create: true})
	must(t, err)
	defer h.Close()

	// want mirrors the file in memory.
	var want []byte
	writeAt := func(data []byte, off int64) {
		t.Helper()
		if _, err := h.WriteAt(data, off); err != nil {
			t.Fatal(err)
		}
		if end := off + int64(len(data)); end > int64(len(want)) {
			want = append(want, make([]byte, end-int64(len(want)))...)
		}
		copy(want[off:], data)
	}
	truncate := func(size int64) {
		t.Helper()
		must(t, h.Truncate(ctx, size))
		if size < int64(len(want)) {
			want = want[:size]
		} else {
			want = append(want, make([]byte, size-int64(len(want)))...)
		}
	}
	verify := func(step string) {
		t.Helper()
		info, err := h.Stat(ctx)
		must(t, err)
		if info.Size != int64(len(want)) {
			t.Fatalf("%s: size = %d, want %d", step, info.Size, len(want))
		}
		got := make([]byte, len(want)+10)
		n, err := h.ReadAt(got, 0)
		if err != io.EOF || n != len(want) || !bytes.Equal(got[:n], want) {
			t.Fatalf("%s: full read = %d, %v, equal=%v", step, n, err, bytes.Equal(got[:n], want))
		}
		// Reads at odd offsets and lengths that straddle boundaries.
		for _, r := range [][2]int64{{cs - 1, 2}, {cs - 7, cs + 14}, {1, 2*cs - 2}, {2*cs + 5, 3}, {0, 1}} {
			off, length := r[0], r[1]
			if off >= int64(len(want)) {
				continue
			}
			buf := make([]byte, length)
			n, err := h.ReadAt(buf, off)
			end := min(off+length, int64(len(want)))
			if n != int(end-off) || !bytes.Equal(buf[:n], want[off:end]) || (end < off+length) != (err == io.EOF) {
				t.Fatalf("%s: ReadAt(%d, %d) = %d, %v", step, length, off, n, err)
			}
		}
	}

	writeAt(pattern(10), cs-5) // straddles chunks 0 and 1, chunk 0 sparse below it
	verify("straddle")
	writeAt(pattern(2*cs), cs/2) // ends mid chunk 2, covers chunk 1 fully
	verify("multi-chunk")
	writeAt([]byte("Z"), 5*cs+3) // gap: chunks 3 and 4 never written
	verify("sparse gap")
	gap := make([]byte, 2*cs)
	if n, err := h.ReadAt(gap, 3*cs); n != len(gap) || err != nil || !bytes.Equal(gap, make([]byte, 2*cs)) {
		t.Fatalf("sparse gap read = %d, %v; want zeros", n, err)
	}
	truncate(cs + 100) // mid chunk 1: drops chunks 2..5, trims chunk 1
	verify("truncate mid-chunk")
	truncate(3 * cs) // grow: the bytes past the old end must read as zeros
	verify("grow after shrink")
	truncate(2 * cs) // exactly on a boundary
	verify("truncate on boundary")
	writeAt(pattern(cs), 2*cs) // an exact, aligned whole chunk
	verify("aligned chunk")
	truncate(0)
	verify("truncate to zero")
}

// TestRemoveWhileOpen checks POSIX-style unlinking: a handle keeps reading
// a file whose directory was removed recursively, the last Close deletes
// the row, and rows left behind by a crash are cleaned up on reopen.
func TestRemoveWhileOpen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fs.db")
	f, err := sqlitefs.Open(ctx, path)
	must(t, err)
	must(t, simplefs.MkdirAll(ctx, f, "/d/sub", 0))
	must(t, simplefs.WriteFile(ctx, f, "/d/sub/f", []byte("still here"), 0))

	h, err := f.Open(ctx, "/d/sub/f", simplefs.OpenOptions{Read: true})
	must(t, err)
	must(t, f.Remove(ctx, "/d", simplefs.RemoveOptions{Recursive: true}))
	buf := make([]byte, 10)
	if n, err := h.ReadAt(buf, 0); n != 10 || string(buf) != "still here" {
		t.Fatalf("read after remove = %d, %v, %q", n, err, buf[:n])
	}
	if info, err := h.Stat(ctx); err != nil || info.Path != "/d/sub/f" {
		t.Fatalf("Stat after remove = %+v, %v; want last path /d/sub/f", info, err)
	}
	must(t, h.Close())
	if n := countNodes(t, path); n != 1 {
		t.Fatalf("%d nodes after last Close, want only the root", n)
	}

	// A "crash": the handle is never closed.
	must(t, simplefs.WriteFile(ctx, f, "/g", []byte("x"), 0))
	h, err = f.Open(ctx, "/g", simplefs.OpenOptions{Read: true})
	must(t, err)
	must(t, f.Remove(ctx, "/g", simplefs.RemoveOptions{}))
	_ = h
	must(t, f.Close())
	if n := countNodes(t, path); n != 2 {
		t.Fatalf("%d nodes before reopen, want root + orphan", n)
	}
	f, err = sqlitefs.Open(ctx, path)
	must(t, err)
	must(t, f.Close())
	if n := countNodes(t, path); n != 1 {
		t.Fatalf("%d nodes after reopen, want only the root", n)
	}
}

// countNodes counts the rows of the nodes table.
func countNodes(t *testing.T, path string) int64 {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.New(ctx, sqlite.WithFile(path), sqlite.WithPoolSize(1))
	must(t, err)
	defer db.Close()
	conn, err := db.Conn(ctx)
	must(t, err)
	defer conn.Done()
	stmt, err := conn.Prepare(ctx, "SELECT count(*) FROM nodes")
	must(t, err)
	if _, err := stmt.Step(); err != nil {
		t.Fatal(err)
	}
	defer stmt.Reset()
	return stmt.ColumnInt64(0)
}

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
