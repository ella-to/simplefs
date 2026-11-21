package fileprovider

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"sync"
)

// journal persists the bridge's ID<->path table, so items keep their IDs
// when the system relaunches the extension: without it every relaunch
// renumbers everything, the system must resync from scratch, and requests
// for IDs it still holds fail with "no such item".
//
// The file is an append-only log, one operation per line (paths quoted with
// strconv.Quote):
//
//	A sfs-12 "/docs/a.txt"     ID assigned
//	R "/docs" "/papers"         renamed (with its subtree)
//	F "/papers/a.txt"           forgotten (deleted)
//
// It is compacted into one A line per live entry when opened and whenever
// it grows past a few times the table's size.
type journal struct {
	mu    sync.Mutex
	path  string
	f     *os.File
	w     *bufio.Writer
	lines int
}

// Persist loads the ID table from the journal file at path (if it exists)
// and records every later change there. Call it once, before serving the
// system's requests; the capi bridge does so with a file under HostDir per
// domain.
func (b *Bridge) Persist(path string) error {
	j := &journal{path: path}
	maxSeq, err := j.replay(b)
	if err != nil {
		return err
	}
	if b.seq.Load() < maxSeq {
		b.seq.Store(maxSeq)
	}
	if err := j.compact(b); err != nil {
		return err
	}
	b.journal.Store(j)
	return nil
}

// replay applies the journal at j.path to b's table and returns the highest
// ID sequence number seen.
func (j *journal) replay(b *Bridge) (uint64, error) {
	f, err := os.Open(j.path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var maxSeq uint64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		op, rest, _ := strings.Cut(sc.Text(), " ")
		switch op {
		case "A":
			id, qp, ok := strings.Cut(rest, " ")
			p, err := strconv.Unquote(qp)
			if !ok || err != nil {
				continue // a torn last line from a crash
			}
			b.ids.Restore(p, id)
			if n, err := strconv.ParseUint(strings.TrimPrefix(id, idPrefix), 10, 64); err == nil && n > maxSeq {
				maxSeq = n
			}
		case "R":
			oldQ, newQ, ok := cutQuoted(rest)
			if ok {
				b.ids.Rekey(oldQ, newQ)
			}
		case "F":
			if p, err := strconv.Unquote(rest); err == nil {
				b.ids.Forget(p)
			}
		}
	}
	return maxSeq, sc.Err()
}

// cutQuoted splits `"a" "b"` into its two unquoted strings.
func cutQuoted(s string) (string, string, bool) {
	first, err := strconv.QuotedPrefix(s)
	if err != nil {
		return "", "", false
	}
	a, _ := strconv.Unquote(first)
	b, err := strconv.Unquote(strings.TrimPrefix(s[len(first):], " "))
	return a, b, err == nil
}

// compact rewrites the journal as one A line per live entry.
func (j *journal) compact(b *Bridge) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	tmp := j.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	n := 0
	b.ids.Each(func(p, id string) {
		fmt.Fprintf(w, "A %s %s\n", id, strconv.Quote(p))
		n++
	})
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Close the live log first: Windows refuses to replace an open file.
	if j.f != nil {
		j.f.Close()
		j.f = nil
	}
	if err := os.Rename(tmp, j.path); err != nil {
		return err
	}
	j.f, err = os.OpenFile(j.path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	j.w = bufio.NewWriter(j.f)
	j.lines = n
	return nil
}

// close closes the log file.
func (j *journal) close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return nil
	}
	err := j.f.Close()
	j.f = nil
	return err
}

// record appends one operation and flushes it.
func (j *journal) record(b *Bridge, format string, args ...any) {
	j.mu.Lock()
	fmt.Fprintf(j.w, format+"\n", args...)
	j.w.Flush()
	j.lines++
	grow := j.lines > 4*b.ids.Len()+1024
	j.mu.Unlock()
	if grow {
		j.compact(b)
	}
}

func (b *Bridge) logAssign(id, path string) {
	if j := b.journal.Load(); j != nil {
		j.record(b, "A %s %s", id, strconv.Quote(path))
	}
}

func (b *Bridge) logRekey(oldPath, newPath string) {
	if j := b.journal.Load(); j != nil {
		j.record(b, "R %s %s", strconv.Quote(oldPath), strconv.Quote(newPath))
	}
}

func (b *Bridge) logForget(path string) {
	if j := b.journal.Load(); j != nil {
		j.record(b, "F %s", strconv.Quote(path))
	}
}
