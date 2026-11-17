// Package sqlitefs is a simplefs.FileProvider stored in one SQLite database
// (via ella.to/sqlite, a pure-Go driver: no cgo). It is an example of a
// database-backed provider and of the design choices that make one fast and
// correct:
//
//   - The tree is a table of nodes, each holding its parent's id and its
//     own name (migrations/001_init.sql). A path is resolved by walking its
//     segments, one indexed lookup each. Because no row stores a full path,
//     a rename is one UPDATE of (parent_id, name) however large the moved
//     subtree is — with path keys it would rewrite every descendant row.
//     Ids also make good inode numbers (FileInfo.FileID) and let open
//     handles follow their file across renames.
//   - File content lives in fixed ChunkSize chunks, so ReadAt and WriteAt
//     touch only the chunks they overlap and a missing chunk reads as zeros
//     (sparse files are free).
//   - Every mutation is one transaction that also bumps the node's version
//     (the ETag) and mtime — and its parent directory's, when an entry is
//     created, removed or renamed. Readers see consistent snapshots.
//   - Recursive remove is one DELETE: foreign keys with ON DELETE CASCADE
//     take the subtree and its chunks with it.
//
// Beyond the core interface it implements symlinks, chmod, chtimes and
// statfs. Hard links, xattrs, ownership and watching are not implemented
// (the schema could grow them: a links table, an xattrs table, ...).
package sqlitefs

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"ella.to/simplefs"
	"ella.to/sqlite"
)

// ChunkSize is the size of the blocks file content is stored in. Large
// enough that sequential I/O costs few rows, small enough that a small
// random write rewrites little.
const ChunkSize = 64 << 10

// rootID is the id of "/", created by New.
const rootID = 1

//go:embed migrations/*.sql
var migrations embed.FS

// FS is a filesystem stored in an SQLite database. Use New or Open. All
// methods are safe for concurrent use.
type FS struct {
	db     *sqlite.Database
	ownsDB bool // Close closes db (set by Open)

	// mu serializes mutations: each holds it for its whole transaction.
	// SQLite admits one writer at a time anyway; waiting here instead of on
	// SQLite's lock keeps the wait cheap and ordered. Reads take no lock and
	// run concurrently on their own pooled connections. Open of an existing
	// file takes the read side, so that resolving the path and registering
	// the handle cannot interleave with a mutation (see unlink).
	mu sync.RWMutex

	// refs counts open handles per node id; orphans holds the ids of nodes
	// unlinked while a handle had them open. Guarded by refMu.
	refMu   sync.Mutex
	refs    map[int64]int
	orphans map[int64]bool
}

var (
	_ simplefs.FileProvider    = (*FS)(nil)
	_ simplefs.SymlinkProvider = (*FS)(nil)
	_ simplefs.ChmodProvider   = (*FS)(nil)
	_ simplefs.ChtimesProvider = (*FS)(nil)
	_ simplefs.StatFSProvider  = (*FS)(nil)
)

// New returns a filesystem stored in db: it applies the schema migrations
// and creates the root directory if needed. db's connections must have
// foreign keys enabled (ella.to/sqlite does this; see also PrepareConn).
// Closing db is up to the caller.
func New(ctx context.Context, db *sqlite.Database) (*FS, error) {
	mfs, err := migrationFS()
	if err != nil {
		return nil, err
	}
	if err := sqlite.Migration(ctx, db, mfs, "."); err != nil {
		return nil, fmt.Errorf("sqlitefs: migrate: %w", err)
	}
	f := &FS{db: db, refs: map[int64]int{}, orphans: map[int64]bool{}}
	err = f.tx(ctx, true, func(conn *sqlite.Conn) error {
		var fk int64
		if _, err := queryRow(ctx, conn, func(s *sqlite.Stmt) { fk = s.ColumnInt64(0) }, "PRAGMA foreign_keys"); err != nil {
			return err
		}
		if fk != 1 {
			return errors.New("foreign keys are disabled on this connection (see PrepareConn)")
		}
		if err := exec(ctx, conn,
			"INSERT OR IGNORE INTO nodes (id, parent_id, name, type, mode, mtime) VALUES (?, NULL, '', ?, ?, ?)",
			rootID, int64(simplefs.TypeDir), int64(0o755), time.Now().UnixNano()); err != nil {
			return err
		}
		// Files removed while open are kept until their last handle closes.
		// A process that died with such handles left them behind.
		return exec(ctx, conn, "DELETE FROM nodes WHERE parent_id IS NULL AND id <> ?", rootID)
	})
	if err != nil {
		return nil, fmt.Errorf("sqlitefs: init: %w", err)
	}
	return f, nil
}

// Close closes the database if the FS was created by Open; otherwise it
// does nothing. Open handles must be closed first.
func (f *FS) Close() error {
	if f.ownsDB {
		return f.db.Close()
	}
	return nil
}

// write runs fn as one write transaction, serialized with all other
// mutations.
func (f *FS) write(ctx context.Context, fn func(conn *sqlite.Conn) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tx(ctx, true, fn)
}

// read runs fn in a read transaction (a consistent snapshot).
func (f *FS) read(ctx context.Context, fn func(conn *sqlite.Conn) error) error {
	return f.tx(ctx, false, fn)
}

// node is one row of the nodes table.
type node struct {
	id, parent int64
	name       string
	typ        simplefs.FileType
	mode       simplefs.FileMode
	size       int64
	mtime      int64
	version    int64
	target     string
}

const nodeCols = "id, parent_id, name, type, mode, size, mtime, version, target"

func scanNode(s *sqlite.Stmt) node {
	return node{
		id:      s.ColumnInt64(0),
		parent:  s.ColumnInt64(1),
		name:    s.ColumnText(2),
		typ:     simplefs.FileType(s.ColumnInt64(3)),
		mode:    simplefs.FileMode(s.ColumnInt64(4)),
		size:    s.ColumnInt64(5),
		mtime:   s.ColumnInt64(6),
		version: s.ColumnInt64(7),
		target:  s.ColumnText(8),
	}
}

func (n node) info(path string) simplefs.FileInfo {
	return simplefs.FileInfo{
		Path:    path,
		Type:    n.typ,
		Size:    n.size,
		Mode:    n.mode,
		ModTime: time.Unix(0, n.mtime),
		ETag:    strconv.FormatInt(n.version, 10),
		FileID:  uint64(n.id),
		Links:   1,
	}
}

// getNode loads the node with the given id; it fails with ErrNotExist when
// there is none.
func getNode(ctx context.Context, conn *sqlite.Conn, id int64) (node, error) {
	var n node
	found, err := queryRow(ctx, conn, func(s *sqlite.Stmt) { n = scanNode(s) },
		"SELECT "+nodeCols+" FROM nodes WHERE id = ?", id)
	if err == nil && !found {
		err = simplefs.ErrNotExist
	}
	return n, err
}

// child looks up the entry name in directory parent.
func child(ctx context.Context, conn *sqlite.Conn, parent int64, name string) (node, bool, error) {
	var n node
	found, err := queryRow(ctx, conn, func(s *sqlite.Stmt) { n = scanNode(s) },
		"SELECT "+nodeCols+" FROM nodes WHERE parent_id = ? AND name = ?", parent, name)
	return n, found, err
}

// lookup resolves the clean path p by walking its segments from the root,
// one indexed (parent_id, name) lookup each. Symlinks are not followed. A
// segment below a non-directory is ErrNotDir.
func lookup(ctx context.Context, conn *sqlite.Conn, p string) (node, error) {
	if p == "/" {
		return getNode(ctx, conn, rootID)
	}
	cur := node{id: rootID, typ: simplefs.TypeDir}
	for seg := range strings.SplitSeq(p[1:], "/") {
		if !cur.typ.IsDir() {
			return node{}, simplefs.ErrNotDir
		}
		next, found, err := child(ctx, conn, cur.id, seg)
		if err != nil {
			return node{}, err
		}
		if !found {
			return node{}, simplefs.ErrNotExist
		}
		cur = next
	}
	return cur, nil
}

// lookupParent resolves the directory that contains the clean path p and
// validates p's final name, which it returns. The root has no parent
// (ErrInvalid).
func lookupParent(ctx context.Context, conn *sqlite.Conn, p string) (node, string, error) {
	if p == "/" {
		return node{}, "", simplefs.ErrInvalid
	}
	parent, err := lookup(ctx, conn, simplefs.ParentPath(p))
	if err != nil {
		return node{}, "", err
	}
	if !parent.typ.IsDir() {
		return node{}, "", simplefs.ErrNotDir
	}
	name := simplefs.BaseName(p)
	if err := simplefs.ValidName(name); err != nil {
		return node{}, "", err
	}
	return parent, name, nil
}

// nodePath computes the current path of node id from its parent chain (one
// recursive query). ok is false when the chain does not reach the root: the
// node was unlinked while open.
func nodePath(ctx context.Context, conn *sqlite.Conn, id int64) (p string, ok bool, err error) {
	var names []string
	top := int64(0)
	err = query(ctx, conn, func(s *sqlite.Stmt) error {
		if top == 0 {
			top = s.ColumnInt64(0)
		} else {
			names = append(names, s.ColumnText(1))
		}
		return nil
	}, `WITH RECURSIVE up(id, parent_id, name, depth) AS (
			SELECT id, parent_id, name, 0 FROM nodes WHERE id = ?
			UNION ALL
			SELECT n.id, n.parent_id, n.name, up.depth + 1 FROM nodes n JOIN up ON n.id = up.parent_id
		)
		SELECT id, name FROM up ORDER BY depth DESC`, id)
	if err != nil {
		return "", false, err
	}
	if top == 0 {
		return "", false, simplefs.ErrNotExist
	}
	if top != rootID {
		return "", false, nil
	}
	return "/" + strings.Join(names, "/"), true, nil
}

// insertNode creates an entry in directory parent and returns it.
func insertNode(ctx context.Context, conn *sqlite.Conn, parent int64, name string, typ simplefs.FileType, mode simplefs.FileMode, target string) (node, error) {
	now := time.Now().UnixNano()
	n := node{parent: parent, name: name, typ: typ, mode: mode.Perm(), mtime: now, version: 1}
	var tgt any // NULL unless a symlink
	if typ.IsSymlink() {
		n.target, n.size, tgt = target, int64(len(target)), target
	}
	_, err := queryRow(ctx, conn, func(s *sqlite.Stmt) { n.id = s.ColumnInt64(0) },
		`INSERT INTO nodes (parent_id, name, type, mode, size, mtime, version, target)
		VALUES (?, ?, ?, ?, ?, ?, 1, ?) RETURNING id`,
		parent, name, int64(typ), int64(n.mode), n.size, now, tgt)
	if err != nil {
		return node{}, err
	}
	return n, touch(ctx, conn, now, parent)
}

// touch records a change of the listed nodes: new mtime, next version.
func touch(ctx context.Context, conn *sqlite.Conn, now int64, ids ...int64) error {
	for _, id := range ids {
		if err := exec(ctx, conn, "UPDATE nodes SET mtime = ?, version = version + 1 WHERE id = ?", now, id); err != nil {
			return err
		}
	}
	return nil
}

// hasChildren reports whether directory id has any entry.
func hasChildren(ctx context.Context, conn *sqlite.Conn, id int64) (bool, error) {
	return queryRow(ctx, conn, func(*sqlite.Stmt) {}, "SELECT 1 FROM nodes WHERE parent_id = ? LIMIT 1", id)
}

// unlink deletes node n and, through ON DELETE CASCADE, everything below it
// and all their chunks — one statement, however big the subtree. (SQLite
// limits cascade recursion to its trigger depth, 1000 levels by default.)
//
// A file that a handle still has open is detached first instead (parent_id
// set to NULL) so the handle keeps working, as on POSIX: rename-over-target
// must not break a reader of the old target. The last Close deletes it.
// Callers hold f.mu, so no Open can register a handle meanwhile.
func (f *FS) unlink(ctx context.Context, conn *sqlite.Conn, n node) error {
	f.refMu.Lock()
	open := make([]int64, 0, len(f.refs))
	for id := range f.refs {
		open = append(open, id)
	}
	f.refMu.Unlock()

	for _, id := range open {
		inside := id == n.id
		if !inside && n.typ.IsDir() {
			var err error
			inside, err = queryRow(ctx, conn, func(*sqlite.Stmt) {},
				`WITH RECURSIVE up(id) AS (
					SELECT parent_id FROM nodes WHERE id = ?
					UNION ALL
					SELECT n.parent_id FROM nodes n JOIN up ON n.id = up.id WHERE n.parent_id IS NOT NULL
				)
				SELECT 1 FROM up WHERE id = ? LIMIT 1`, id, n.id)
			if err != nil {
				return err
			}
		}
		if !inside {
			continue
		}
		if err := exec(ctx, conn, "UPDATE nodes SET parent_id = NULL WHERE id = ?", id); err != nil {
			return err
		}
		// Mark it under the same lock release checks, so the last Close
		// either sees the mark (and deletes the node) or has already
		// happened (and we delete it here).
		f.refMu.Lock()
		stillOpen := f.refs[id] > 0
		if stillOpen {
			f.orphans[id] = true
		}
		f.refMu.Unlock()
		if !stillOpen {
			if err := exec(ctx, conn, "DELETE FROM nodes WHERE id = ?", id); err != nil {
				return err
			}
		}
		if id == n.id {
			return nil // n itself was the open file: nothing left to delete
		}
	}
	return exec(ctx, conn, "DELETE FROM nodes WHERE id = ?", n.id)
}

// retain registers an open handle on node id.
func (f *FS) retain(id int64) {
	f.refMu.Lock()
	f.refs[id]++
	f.refMu.Unlock()
}

// release drops a handle on node id; the last one deletes the node if it
// was unlinked while open.
func (f *FS) release(ctx context.Context, id int64) error {
	f.refMu.Lock()
	f.refs[id]--
	last := f.refs[id] == 0
	orphan := last && f.orphans[id]
	if last {
		delete(f.refs, id)
		delete(f.orphans, id)
	}
	f.refMu.Unlock()
	if !orphan {
		return nil
	}
	// The parent_id guard keeps this safe even if the transaction that
	// detached the node was rolled back.
	return f.write(ctx, func(conn *sqlite.Conn) error {
		return exec(ctx, conn, "DELETE FROM nodes WHERE id = ? AND parent_id IS NULL AND id <> ?", id, rootID)
	})
}

// Stat returns metadata for path without following a final symlink.
func (f *FS) Stat(ctx context.Context, path string) (simplefs.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return simplefs.FileInfo{}, err
	}
	p := simplefs.CleanPath(path)
	var n node
	err := f.read(ctx, func(conn *sqlite.Conn) (err error) {
		n, err = lookup(ctx, conn, p)
		return err
	})
	if err != nil {
		return simplefs.FileInfo{}, patherr("stat", p, err)
	}
	return n.info(p), nil
}

// Open opens the file at path; see simplefs.FileProvider.
func (f *FS) Open(ctx context.Context, path string, opts simplefs.OpenOptions) (simplefs.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := simplefs.CleanPath(path)
	if !opts.Read && !opts.Write {
		opts.Read = true
	}
	if (opts.Truncate || opts.Append) && !opts.Write {
		return nil, patherr("open", p, simplefs.ErrInvalid)
	}

	// Create and Truncate may change the database; a plain open only reads,
	// but still excludes mutations until the handle is registered.
	mutating := opts.Create || opts.Truncate
	if mutating {
		f.mu.Lock()
		defer f.mu.Unlock()
	} else {
		f.mu.RLock()
		defer f.mu.RUnlock()
	}

	var n node
	err := f.tx(ctx, mutating, func(conn *sqlite.Conn) (err error) {
		n, err = lookup(ctx, conn, p)
		switch {
		case err == nil:
			if opts.Create && opts.Exclusive {
				return simplefs.ErrExist
			}
		case errors.Is(err, simplefs.ErrNotExist) && opts.Create:
			parent, name, err := lookupParent(ctx, conn, p)
			if err != nil {
				return err
			}
			mode := opts.Mode
			if mode == 0 {
				mode = 0o644
			}
			n, err = insertNode(ctx, conn, parent.id, name, simplefs.TypeFile, mode, "")
			return err
		default:
			return err
		}
		switch {
		case n.typ.IsDir():
			return simplefs.ErrIsDir
		case n.typ.IsSymlink():
			return simplefs.ErrNotSupported // like memfs; callers resolve links themselves
		}
		if opts.Truncate && n.size > 0 {
			now := time.Now().UnixNano()
			if err := exec(ctx, conn, "UPDATE nodes SET size = 0, mtime = ?, version = version + 1 WHERE id = ?", now, n.id); err != nil {
				return err
			}
			return exec(ctx, conn, "DELETE FROM chunks WHERE node_id = ?", n.id)
		}
		return nil
	})
	if err != nil {
		return nil, patherr("open", p, err)
	}
	f.retain(n.id)
	return &handle{f: f, id: n.id, opts: opts, path: p}, nil
}

// ReadDir lists the directory at path, sorted by name. Pages are keyset
// paginated: Page.Token is the last name of the previous page, and the next
// page is `name > token` on the (parent_id, name) index — no OFFSET scan,
// and entries added or removed between pages cannot cause a repeat. Each
// entry carries its full Info, read by the same query.
func (f *FS) ReadDir(ctx context.Context, path string, page simplefs.Page) (simplefs.DirPage, error) {
	if err := ctx.Err(); err != nil {
		return simplefs.DirPage{}, err
	}
	p := simplefs.CleanPath(path)
	limit := int64(-1) // no limit
	if page.Limit > 0 {
		limit = int64(page.Limit) + 1 // one extra row tells whether another page exists
	}
	var out simplefs.DirPage
	err := f.read(ctx, func(conn *sqlite.Conn) error {
		dir, err := lookup(ctx, conn, p)
		if err != nil {
			return err
		}
		if !dir.typ.IsDir() {
			return simplefs.ErrNotDir
		}
		return query(ctx, conn, func(s *sqlite.Stmt) error {
			n := scanNode(s)
			info := n.info(simplefs.JoinPath(p, n.name))
			out.Entries = append(out.Entries, simplefs.DirEntry{Name: n.name, Type: n.typ, Info: &info})
			return nil
		}, "SELECT "+nodeCols+" FROM nodes WHERE parent_id = ? AND name > ? ORDER BY name LIMIT ?",
			dir.id, page.Token, limit)
	})
	if err != nil {
		return simplefs.DirPage{}, patherr("readdir", p, err)
	}
	if page.Limit > 0 && len(out.Entries) > page.Limit {
		out.Entries = out.Entries[:page.Limit]
		out.NextToken = out.Entries[page.Limit-1].Name
	}
	return out, nil
}

// Mkdir creates a directory; the parent must exist.
func (f *FS) Mkdir(ctx context.Context, path string, opts simplefs.MkdirOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p := simplefs.CleanPath(path)
	mode := opts.Mode
	if mode == 0 {
		mode = 0o755
	}
	err := f.write(ctx, func(conn *sqlite.Conn) error {
		parent, name, err := lookupParent(ctx, conn, p)
		if err != nil {
			return err
		}
		_, err = insertNode(ctx, conn, parent.id, name, simplefs.TypeDir, mode, "")
		return err // a taken name violates UNIQUE (parent_id, name): ErrExist
	})
	if err != nil {
		return patherr("mkdir", p, err)
	}
	return nil
}

// Remove removes a file, symlink, or directory. With opts.Recursive a
// non-empty directory goes with all its contents, in one DELETE.
func (f *FS) Remove(ctx context.Context, path string, opts simplefs.RemoveOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p := simplefs.CleanPath(path)
	if p == "/" {
		return patherr("remove", p, simplefs.ErrInvalid)
	}
	err := f.write(ctx, func(conn *sqlite.Conn) error {
		parent, name, err := lookupParent(ctx, conn, p)
		if err != nil {
			return err
		}
		n, found, err := child(ctx, conn, parent.id, name)
		if err != nil {
			return err
		}
		if !found {
			return simplefs.ErrNotExist
		}
		if n.typ.IsDir() && !opts.Recursive {
			if nonEmpty, err := hasChildren(ctx, conn, n.id); err != nil || nonEmpty {
				return cmpOr(err, simplefs.ErrNotEmpty)
			}
		}
		if err := f.unlink(ctx, conn, n); err != nil {
			return err
		}
		return touch(ctx, conn, time.Now().UnixNano(), parent.id)
	})
	if err != nil {
		if opts.Force && errors.Is(err, simplefs.ErrNotExist) {
			return nil
		}
		return patherr("remove", p, err)
	}
	return nil
}

// Rename moves oldPath to newPath. It is a single UPDATE of the node's
// (parent_id, name): descendants refer to the node by id, so a directory's
// whole subtree moves with it untouched, and open handles keep working.
// Overwriting deletes the destination in the same transaction, so readers
// see either the old or the new file, never neither.
func (f *FS) Rename(ctx context.Context, oldPath, newPath string, opts simplefs.RenameOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	op, np := simplefs.CleanPath(oldPath), simplefs.CleanPath(newPath)
	if op == "/" || np == "/" {
		return patherr("rename", op, simplefs.ErrInvalid)
	}
	if op == np {
		return nil
	}
	if simplefs.IsUnder(op, np) {
		return patherr("rename", np, simplefs.ErrInvalid) // a directory into its own subtree
	}
	errPath := op
	err := f.write(ctx, func(conn *sqlite.Conn) error {
		srcParent, srcName, err := lookupParent(ctx, conn, op)
		if err != nil {
			return err
		}
		src, found, err := child(ctx, conn, srcParent.id, srcName)
		if err != nil {
			return err
		}
		if !found {
			return simplefs.ErrNotExist
		}
		errPath = np
		dstParent, dstName, err := lookupParent(ctx, conn, np)
		if err != nil {
			return err
		}
		dst, replacing, err := child(ctx, conn, dstParent.id, dstName)
		if err != nil {
			return err
		}
		if replacing {
			switch {
			case !opts.Overwrite:
				return simplefs.ErrExist
			case dst.typ.IsDir() && !src.typ.IsDir():
				return simplefs.ErrIsDir
			case !dst.typ.IsDir() && src.typ.IsDir():
				return simplefs.ErrNotDir
			}
			if dst.typ.IsDir() {
				if nonEmpty, err := hasChildren(ctx, conn, dst.id); err != nil || nonEmpty {
					return cmpOr(err, simplefs.ErrNotEmpty)
				}
			}
			if err := f.unlink(ctx, conn, dst); err != nil {
				return err
			}
		}
		now := time.Now().UnixNano()
		if err := exec(ctx, conn, "UPDATE nodes SET parent_id = ?, name = ?, version = version + 1 WHERE id = ?",
			dstParent.id, dstName, src.id); err != nil {
			return err
		}
		if dstParent.id == srcParent.id {
			return touch(ctx, conn, now, srcParent.id)
		}
		return touch(ctx, conn, now, srcParent.id, dstParent.id)
	})
	if err != nil {
		return patherr("rename", errPath, err)
	}
	return nil
}

// Readlink returns the target of the symlink at path.
func (f *FS) Readlink(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p := simplefs.CleanPath(path)
	var n node
	err := f.read(ctx, func(conn *sqlite.Conn) (err error) {
		n, err = lookup(ctx, conn, p)
		return err
	})
	if err == nil && !n.typ.IsSymlink() {
		err = simplefs.ErrInvalid
	}
	if err != nil {
		return "", patherr("readlink", p, err)
	}
	return n.target, nil
}

// Symlink creates a symbolic link at linkPath pointing to target. The target
// is stored as given and need not exist.
func (f *FS) Symlink(ctx context.Context, target, linkPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p := simplefs.CleanPath(linkPath)
	err := f.write(ctx, func(conn *sqlite.Conn) error {
		parent, name, err := lookupParent(ctx, conn, p)
		if err != nil {
			return err
		}
		_, err = insertNode(ctx, conn, parent.id, name, simplefs.TypeSymlink, 0o777, target)
		return err
	})
	if err != nil {
		return patherr("symlink", p, err)
	}
	return nil
}

// Chmod sets the permission bits of the object at path.
func (f *FS) Chmod(ctx context.Context, path string, mode simplefs.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.setMeta(ctx, "chmod", path, "mode = ?", int64(mode.Perm()))
}

// Chtimes sets the modification time of the object at path. A zero mtime
// leaves it unchanged; atime is not stored.
func (f *FS) Chtimes(ctx context.Context, path string, atime, mtime time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if mtime.IsZero() {
		return f.setMeta(ctx, "chtimes", path, "mtime = mtime")
	}
	return f.setMeta(ctx, "chtimes", path, "mtime = ?", mtime.UnixNano())
}

// setMeta applies `set` to the node at path and bumps its version.
func (f *FS) setMeta(ctx context.Context, op, path, set string, args ...any) error {
	p := simplefs.CleanPath(path)
	err := f.write(ctx, func(conn *sqlite.Conn) error {
		n, err := lookup(ctx, conn, p)
		if err != nil {
			return err
		}
		return exec(ctx, conn, "UPDATE nodes SET "+set+", version = version + 1 WHERE id = ?", append(args, n.id)...)
	})
	if err != nil {
		return patherr(op, p, err)
	}
	return nil
}

// StatFS reports the database's size in SQLite pages. The capacity is
// SQLite's own limit (max_page_count, terabytes by default); free pages
// inside the file count as free.
func (f *FS) StatFS(ctx context.Context) (simplefs.FSInfo, error) {
	if err := ctx.Err(); err != nil {
		return simplefs.FSInfo{}, err
	}
	var pageSize, pages, freePages, maxPages int64
	err := f.read(ctx, func(conn *sqlite.Conn) error {
		for _, q := range []struct {
			pragma string
			dst    *int64
		}{
			{"PRAGMA page_size", &pageSize},
			{"PRAGMA page_count", &pages},
			{"PRAGMA freelist_count", &freePages},
			{"PRAGMA max_page_count", &maxPages},
		} {
			if _, err := queryRow(ctx, conn, func(s *sqlite.Stmt) { *q.dst = s.ColumnInt64(0) }, q.pragma); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return simplefs.FSInfo{}, patherr("statfs", "/", err)
	}
	used := max(pages-freePages, 0)
	total := max(maxPages, pages)
	return simplefs.FSInfo{
		BlockSize:     uint64(pageSize),
		Blocks:        uint64(total),
		Free:          uint64(total - used),
		Available:     uint64(total - used),
		MaxNameLength: simplefs.MaxNameLength,
	}, nil
}

// cmpOr returns err if it is non-nil, otherwise fallback.
func cmpOr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}
