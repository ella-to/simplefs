package sqlitefs

import (
	"context"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"ella.to/simplefs"
	"ella.to/sqlite"
	zsqlite "zombiezen.com/go/sqlite"
)

// PoolSize is the number of connections Open puts in the pool. Mutations are
// serialized by FS anyway (see FS.mu), so extra connections only let reads —
// Stat, ReadDir, ReadAt — run in parallel with each other and with a write,
// which WAL mode allows.
const PoolSize = 8

// PrepareConn configures every pooled connection. Pass it to
// sqlite.WithConnPrepareFunc when building the database for New yourself.
//
//   - foreign_keys=ON is what makes ON DELETE CASCADE work: removing a
//     directory deletes its whole subtree, and its files' chunks, with one
//     DELETE. SQLite leaves it off by default, per connection.
//     (ella.to/sqlite turns it on too, but correctness depends on it, so it
//     is stated here rather than assumed.)
//   - journal_mode=WAL lets readers proceed while a write is in progress
//     and makes each commit a single append.
//   - synchronous=FULL syncs the WAL on every commit, so every mutation is
//     durable when its method returns and FileHandle.Sync has nothing left
//     to do. NORMAL is faster but can lose the last commits on power loss.
//   - busy_timeout waits for another process's lock instead of failing at
//     once with SQLITE_BUSY.
//
// Each pragma runs as its own statement, not through conn.ExecScript:
// ExecScript wraps the script in a SAVEPOINT, and inside a transaction
// foreign_keys is silently ignored and synchronous is an error.
func PrepareConn(conn *sqlite.Conn) error {
	for _, pragma := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
		"PRAGMA busy_timeout = 10000",
	} {
		if err := exec(context.Background(), conn, pragma); err != nil {
			return err
		}
	}
	return nil
}

// Open opens (creating it if needed) the database file at path and returns a
// filesystem stored in it. Close the FS to close the database.
//
// Only one FS should use a database file at a time: New deletes the leftovers
// of files that were removed while open, which another live FS might still
// be reading.
func Open(ctx context.Context, path string) (*FS, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	uri, err := fileURI(path)
	if err != nil {
		return nil, err
	}
	// sqlite.WithFile would add cache=shared to the URI. In shared-cache
	// mode connections share one page cache and lock whole tables against
	// each other — a reader can then deadlock with a writer (SQLITE_LOCKED)
	// and WAL's concurrent readers are lost. Private caches avoid both.
	db, err := sqlite.New(ctx,
		sqlite.WithStringConn(uri),
		sqlite.WithPoolSize(PoolSize),
		sqlite.WithConnPrepareFunc(PrepareConn),
	)
	if err != nil {
		return nil, err
	}
	f, err := New(ctx, db)
	if err != nil {
		db.Close()
		return nil, err
	}
	f.ownsDB = true
	return f, nil
}

// fileURI turns a file path into an SQLite URI, escaping '?', '#', '%' and
// spaces, which a plain "file:"+path would misread.
func fileURI(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // C:/x -> /C:/x, as SQLite expects on Windows
	}
	return (&url.URL{Scheme: "file", Path: p}).String(), nil
}

// tx runs fn in one transaction on a pooled connection: committed when fn
// returns nil, rolled back otherwise. A write transaction starts with BEGIN
// IMMEDIATE, taking the write lock up front, so it can never fail halfway
// through on a read-to-write lock upgrade. A read transaction gives fn a
// consistent snapshot (a path walk or a multi-chunk read never sees half of
// a concurrent write).
//
// ella.to/sqlite's Conn.Save is not used: written as the documented
// `defer conn.Save(&err)`, the deferred call opens and releases its
// SAVEPOINT only when the function returns, so the statements before it
// run outside any transaction.
//
// The connection is taken with a context that is never cancelled. The pool
// interrupts a connection when its context ends, and an interrupted
// connection also refuses the ROLLBACK — it would go back to the pool still
// inside a transaction. Methods check ctx before starting instead; each
// transaction is short.
func (f *FS) tx(ctx context.Context, write bool, fn func(conn *sqlite.Conn) error) (err error) {
	conn, err := f.db.Conn(context.WithoutCancel(ctx))
	if err != nil {
		return err
	}
	defer conn.Done()

	begin := "BEGIN DEFERRED"
	if write {
		begin = "BEGIN IMMEDIATE"
	}
	if err := exec(ctx, conn, begin); err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = exec(ctx, conn, "ROLLBACK")
			panic(p)
		}
		if err == nil {
			err = exec(ctx, conn, "COMMIT")
		}
		if err != nil {
			_ = exec(ctx, conn, "ROLLBACK") // fails harmlessly if SQLite already rolled back
		}
	}()
	return fn(conn)
}

// exec runs a statement that returns no rows we need.
func exec(ctx context.Context, conn *sqlite.Conn, query string, args ...any) error {
	stmt, err := conn.Prepare(ctx, query, args...)
	if err != nil {
		return err
	}
	for {
		row, err := stmt.Step()
		if err != nil || !row {
			return err
		}
	}
}

// queryRow runs query and calls scan with the first row, reporting whether
// there was one. The statement is reset before returning: Prepare caches
// statements per connection, and the pool refuses a connection with a
// statement still positioned on a row (which would also pin its snapshot).
func queryRow(ctx context.Context, conn *sqlite.Conn, scan func(*sqlite.Stmt), query string, args ...any) (bool, error) {
	stmt, err := conn.Prepare(ctx, query, args...)
	if err != nil {
		return false, err
	}
	row, err := stmt.Step()
	if err != nil {
		return false, err
	}
	if row {
		scan(stmt)
	}
	return row, stmt.Reset()
}

// query calls each for every row of query; an error from each stops the
// iteration and is returned.
func query(ctx context.Context, conn *sqlite.Conn, each func(*sqlite.Stmt) error, query string, args ...any) error {
	stmt, err := conn.Prepare(ctx, query, args...)
	if err != nil {
		return err
	}
	for {
		row, err := stmt.Step()
		if err != nil || !row {
			return err
		}
		if err := each(stmt); err != nil {
			stmt.Reset()
			return err
		}
	}
}

// patherr wraps err for op on path, translating SQLite failures: a UNIQUE
// violation means the name is taken (ErrExist), a full disk or database is
// ErrNoSpace, and a lock that could not be had in time is ErrBusy.
func patherr(op, path string, err error) error {
	switch zsqlite.ErrCode(err).ToPrimary() {
	case zsqlite.ResultConstraint:
		if code := zsqlite.ErrCode(err); code == zsqlite.ResultConstraintUnique || code == zsqlite.ResultConstraintPrimaryKey {
			err = errors.Join(simplefs.ErrExist, err)
		}
	case zsqlite.ResultFull:
		err = errors.Join(simplefs.ErrNoSpace, err)
	case zsqlite.ResultBusy, zsqlite.ResultLocked:
		err = errors.Join(simplefs.ErrBusy, err)
	}
	return &simplefs.PathError{Op: op, Path: path, Err: err}
}

// migrationFS adapts the embedded migrations for sqlite.Migration. It is
// rooted at the migrations directory and passed with dir ".", because
// Migration joins names with filepath.Join: with dir "migrations" it would
// look up (and record) "migrations\001_init.sql" on Windows, which embed.FS
// rejects.
func migrationFS() (sqlite.ReadDirFileFS, error) {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return nil, err
	}
	rd, ok := sub.(sqlite.ReadDirFileFS)
	if !ok {
		return nil, errors.New("sqlitefs: migrations are not a ReadDirFileFS")
	}
	return rd, nil
}
