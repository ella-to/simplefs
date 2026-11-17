-- One row per filesystem object. The tree is an adjacency list: a node
-- knows its parent and its own name, never its full path, so renaming or
-- moving a directory rewrites exactly one row however large its subtree.
--
-- AUTOINCREMENT makes SQLite never reuse the id of a deleted node: ids are
-- reported as inode numbers (FileInfo.FileID), and a recycled number could
-- make a client's cached handle silently point at a different file.
CREATE TABLE nodes (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  -- NULL for the root (id 1) and for unlinked nodes that an open handle
  -- still uses (removed POSIX-style; deleted on the last Close or, after a
  -- crash, the next time the database is opened).
  parent_id INTEGER REFERENCES nodes(id) ON DELETE CASCADE,
  name      TEXT    NOT NULL,
  type      INTEGER NOT NULL,           -- simplefs.FileType
  mode      INTEGER NOT NULL,           -- permission bits
  size      INTEGER NOT NULL DEFAULT 0, -- bytes; len(target) for a symlink
  mtime     INTEGER NOT NULL,           -- unix nanoseconds
  version   INTEGER NOT NULL DEFAULT 1, -- reported as the ETag
  target    TEXT,                       -- symlink target
  -- Also the index behind every path lookup (parent_id = ? AND name = ?),
  -- every ordered, paged directory listing, and the cascade's child search.
  UNIQUE (parent_id, name)
);

-- File content in fixed-size chunks (see ChunkSize). A missing chunk, or the
-- part of the file past a short chunk's end, reads as zeros, so sparse files
-- cost nothing.
CREATE TABLE chunks (
  node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  idx     INTEGER NOT NULL,
  data    BLOB    NOT NULL,
  PRIMARY KEY (node_id, idx)
) WITHOUT ROWID;
