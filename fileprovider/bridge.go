package fileprovider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync/atomic"

	"ella.to/simplefs"
	"ella.to/simplefs/internal/idtable"
)

// Bridge adapts a path-based simplefs.FileProvider onto the ID-based item model the
// File Provider system works with. It is pure Go — the capi package forwards
// the extension's C calls to the bridge registered with Register — so it is
// also fully testable without mounting anything.
//
// Because the core simplefs.FileProvider is fully mutable, every item advertises
// full permissions; a provider can still refuse individual operations by
// returning simplefs.ErrPermission (surfaced in Finder as "not permitted"),
// simplefs.ErrNotSupported, or an *Error for the exact code.
//
// The system addresses items by stable ID while simplefs.FileProvider is
// path-based, so the bridge keeps an ID<->path table, assigning IDs lazily as
// paths are discovered and re-keying them on rename and delete. With Persist
// the table survives extension relaunches; otherwise items receive fresh IDs
// after a restart and the system re-syncs from scratch.
type Bridge struct {
	ctx context.Context
	p   simplefs.FileProvider

	ids     *idtable.Table[string]
	seq     atomic.Uint64           // last "sfs-N" handed out
	journal atomic.Pointer[journal] // set by Persist
	changes changeLog
}

// idPrefix starts every item ID the bridge hands out.
const idPrefix = "sfs-"

// NewBridge wraps p without registering it (useful for tests). Provider calls
// receive context.Background(); the system does not carry a per-request
// context across the extension boundary.
func NewBridge(p simplefs.FileProvider) *Bridge {
	b := &Bridge{ctx: context.Background(), p: p}
	b.ids = idtable.New(RootID, func() string {
		return idPrefix + strconv.FormatUint(b.seq.Add(1), 10)
	})
	return b
}

// Provider returns the wrapped simplefs.FileProvider.
func (b *Bridge) Provider() simplefs.FileProvider { return b.p }

// UpdateRequest describes a change to an existing item, mirroring the
// system's modify call: any combination of new content, a new name, and a new
// parent.
type UpdateRequest struct {
	ID             string
	ChangedContent bool
	ChangedName    bool
	ChangedParent  bool
	NewName        string
	NewParentID    string

	// ContentPath is the local file holding the new content (the URL the
	// system hands the extension); it is streamed into the provider.
	ContentPath string
}

// idFor returns the stable ID for path, assigning (and journaling) one on
// first sight.
func (b *Bridge) idFor(path string) string {
	if id, ok := b.ids.Lookup(path); ok {
		return id
	}
	id := b.ids.ID(path)
	b.logAssign(id, path)
	return id
}

// pathFor resolves an ID handed to us by the system back to a path.
func (b *Bridge) pathFor(id string) (string, error) {
	path, ok := b.ids.Path(id)
	if !ok {
		return "", &Error{Code: CodeNotFound, Msg: "no item with id " + id}
	}
	return path, nil
}

// wrapErr turns a provider error into an *Error so the right NSError —
// and therefore the right Finder alert — reaches the user.
func wrapErr(err error) error {
	if err == nil {
		return nil
	}
	code, msg := CodeOf(err)
	return &Error{Code: code, Msg: msg}
}

// item converts a FileInfo into the wire shape. Symlinks are exposed as plain
// files (File Provider has no symlink concept); their content is whatever the
// provider serves for the path.
func (b *Bridge) item(info simplefs.FileInfo, id string) Item {
	dir := info.Type.IsDir()
	var size int64
	if !dir {
		size = info.Size
	}
	version := info.ETag
	if version == "" {
		version = fmt.Sprintf("%d-%d", info.ModTime.UnixNano(), info.Size)
	}
	parentID := RootID
	if info.Path != "/" {
		parentID = b.idFor(simplefs.ParentPath(info.Path))
	}
	name := info.Name()
	if info.Path == "/" {
		name = "root"
	}
	perms := PermRead | PermWrite | PermRename | PermMove | PermDelete
	if dir {
		perms = PermRead | PermRename | PermMove | PermDelete | PermAddItems
	}
	return Item{
		ID:       id,
		Name:     name,
		Size:     size,
		Dir:      dir,
		Version:  version,
		ParentID: parentID,
		Perms:    perms,
	}
}

// Root returns the top-level container. The host forces its identifier to the
// system root constant and restricts its perms to read + add-items.
func (b *Bridge) Root() (Item, error) {
	info, err := b.p.Stat(b.ctx, "/")
	if err != nil {
		return Item{}, wrapErr(err)
	}
	it := b.item(info, RootID)
	it.Perms = PermRead | PermAddItems
	return it, nil
}

// Stat returns the metadata for a single item.
func (b *Bridge) Stat(id string) (Item, error) {
	path, err := b.pathFor(id)
	if err != nil {
		return Item{}, err
	}
	info, err := b.p.Stat(b.ctx, path)
	if err != nil {
		return Item{}, wrapErr(err)
	}
	return b.item(info, id), nil
}

// ReadDirPage returns one page of a container's children: at most limit
// items (<= 0: all) after token (the previous page's next token; "" for the
// first page). next is "" on the last page. The system enumerates large
// folders page by page this way instead of in one huge call.
func (b *Bridge) ReadDirPage(parentID, token string, limit int) (items []Item, next string, err error) {
	parent, err := b.pathFor(parentID)
	if err != nil {
		return nil, "", err
	}
	page, err := b.p.ReadDir(b.ctx, parent, simplefs.Page{Limit: limit, Token: token})
	if err != nil {
		return nil, "", wrapErr(err)
	}
	for _, e := range page.Entries {
		childPath := simplefs.JoinPath(parent, e.Name)
		var info simplefs.FileInfo
		if e.Info != nil {
			info = *e.Info
			info.Path = childPath
		} else if info, err = b.p.Stat(b.ctx, childPath); err != nil {
			if errors.Is(err, simplefs.ErrNotExist) {
				continue // vanished between ReadDir and Stat
			}
			return nil, "", wrapErr(err)
		}
		items = append(items, b.item(info, b.idFor(childPath)))
	}
	return items, page.NextToken, nil
}

// ReadDir returns the children of a container, draining the provider's
// pagination so the system always sees the complete listing.
func (b *Bridge) ReadDir(parentID string) ([]Item, error) {
	parent, err := b.pathFor(parentID)
	if err != nil {
		return nil, err
	}
	var items []Item
	for e, err := range simplefs.Entries(b.ctx, b.p, parent) {
		if err != nil {
			return nil, wrapErr(err)
		}
		childPath := simplefs.JoinPath(parent, e.Name)
		var info simplefs.FileInfo
		if e.Info != nil {
			info = *e.Info
			info.Path = childPath
		} else {
			info, err = b.p.Stat(b.ctx, childPath)
			if errors.Is(err, simplefs.ErrNotExist) {
				continue // entry vanished between ReadDir and Stat
			}
			if err != nil {
				return nil, wrapErr(err)
			}
		}
		items = append(items, b.item(info, b.idFor(childPath)))
	}
	return items, nil
}

// Open returns a reader over the file's full content, backed by the
// provider's random-access handle. The host reads it fully and closes it.
func (b *Bridge) Open(id string) (io.ReadCloser, error) {
	path, err := b.pathFor(id)
	if err != nil {
		return nil, err
	}
	h, err := b.p.Open(b.ctx, path, simplefs.OpenOptions{Read: true})
	if err != nil {
		return nil, wrapErr(err)
	}
	info, err := h.Stat(b.ctx)
	if err != nil {
		h.Close()
		return nil, wrapErr(err)
	}
	return &handleReader{SectionReader: io.NewSectionReader(h, 0, info.Size), h: h}, nil
}

type handleReader struct {
	*io.SectionReader
	h simplefs.FileHandle
}

func (r *handleReader) Close() error { return r.h.Close() }

// FetchTo streams the content of the file with the given ID into the local
// file dst (created or truncated) in fixed-size chunks — memory use does not
// depend on the file size — and returns the item as of the copy.
func (b *Bridge) FetchTo(id, dst string) (Item, error) {
	path, err := b.pathFor(id)
	if err != nil {
		return Item{}, err
	}
	h, err := b.p.Open(b.ctx, path, simplefs.OpenOptions{Read: true})
	if err != nil {
		return Item{}, wrapErr(err)
	}
	defer h.Close()
	info, err := h.Stat(b.ctx)
	if err != nil {
		return Item{}, wrapErr(err)
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return Item{}, &Error{Code: CodeIO, Msg: err.Error()}
	}
	buf := make([]byte, chunkSize)
	if _, err := io.CopyBuffer(f, io.NewSectionReader(h, 0, info.Size), buf); err != nil {
		f.Close()
		return Item{}, wrapErr(err)
	}
	if err := f.Close(); err != nil {
		return Item{}, &Error{Code: CodeIO, Msg: err.Error()}
	}
	return b.item(info, id), nil
}

// chunkSize is how much content moves per read/write when streaming.
const chunkSize = 1 << 20

// writeFrom streams the local file src (empty: no content) into h from
// offset 0, syncs, and returns the handle's final metadata.
func writeFrom(ctx context.Context, h simplefs.FileHandle, src string) (simplefs.FileInfo, error) {
	if src != "" {
		f, err := os.Open(src)
		if err != nil {
			return simplefs.FileInfo{}, err
		}
		defer f.Close()
		if _, err := io.CopyBuffer(io.NewOffsetWriter(h, 0), f, make([]byte, chunkSize)); err != nil {
			return simplefs.FileInfo{}, err
		}
	}
	if err := h.Sync(ctx); err != nil {
		return simplefs.FileInfo{}, err
	}
	return h.Stat(ctx)
}

// Create adds a new file or folder the user created under parentID. For a
// file, contentPath names the local file holding its content ("" for none).
func (b *Bridge) Create(name, parentID string, dir bool, contentPath string) (Item, error) {
	parent, err := b.pathFor(parentID)
	if err != nil {
		return Item{}, err
	}
	if err := simplefs.ValidName(name); err != nil {
		return Item{}, wrapErr(err)
	}
	path := simplefs.JoinPath(parent, name)

	var info simplefs.FileInfo
	if dir {
		if err := b.p.Mkdir(b.ctx, path, simplefs.MkdirOptions{Mode: 0o755}); err != nil {
			return Item{}, wrapErr(err)
		}
		if info, err = b.p.Stat(b.ctx, path); err != nil {
			return Item{}, wrapErr(err)
		}
	} else {
		// Exclusive: a name collision must surface as a conflict (Finder
		// then picks "name 2") instead of silently replacing the file.
		h, err := b.p.Open(b.ctx, path, simplefs.OpenOptions{
			Read: true, Write: true, Create: true, Exclusive: true, Mode: 0o644,
		})
		if err != nil {
			return Item{}, wrapErr(err)
		}
		if info, err = writeFrom(b.ctx, h, contentPath); err != nil {
			h.Close()
			return Item{}, wrapErr(err)
		}
		if err := h.Close(); err != nil {
			return Item{}, wrapErr(err)
		}
	}
	return b.item(info, b.idFor(path)), nil
}

// Update applies a content edit, rename, and/or move: content first, then
// rename/move, so the final Stat reflects everything.
func (b *Bridge) Update(req UpdateRequest) (Item, error) {
	path, err := b.pathFor(req.ID)
	if err != nil {
		return Item{}, err
	}

	if req.ChangedContent {
		h, err := b.p.Open(b.ctx, path, simplefs.OpenOptions{Write: true, Truncate: true})
		if err != nil {
			return Item{}, wrapErr(err)
		}
		if _, err := writeFrom(b.ctx, h, req.ContentPath); err != nil {
			h.Close()
			return Item{}, wrapErr(err)
		}
		if err := h.Close(); err != nil {
			return Item{}, wrapErr(err)
		}
	}

	if req.ChangedName || req.ChangedParent {
		dir := simplefs.ParentPath(path)
		name := simplefs.BaseName(path)
		if req.ChangedParent {
			if dir, err = b.pathFor(req.NewParentID); err != nil {
				return Item{}, err
			}
		}
		if req.ChangedName {
			if err := simplefs.ValidName(req.NewName); err != nil {
				return Item{}, wrapErr(err)
			}
			name = req.NewName
		}
		if newPath := simplefs.JoinPath(dir, name); newPath != path {
			if err := b.p.Rename(b.ctx, path, newPath, simplefs.RenameOptions{Overwrite: true}); err != nil {
				return Item{}, wrapErr(err)
			}
			b.ids.Rekey(path, newPath)
			b.logRekey(path, newPath)
			path = newPath
		}
	}

	info, err := b.p.Stat(b.ctx, path)
	if err != nil {
		return Item{}, wrapErr(err)
	}
	return b.item(info, req.ID), nil
}

// Delete removes the item with the given ID (directories recursively — that
// is what the user asked for by deleting the folder in Finder).
func (b *Bridge) Delete(id string) error {
	path, err := b.pathFor(id)
	if err != nil {
		return err
	}
	if path == "/" {
		return &Error{Code: CodeNotPermitted, Msg: "cannot delete the root"}
	}
	if err := b.p.Remove(b.ctx, path, simplefs.RemoveOptions{Recursive: true}); err != nil {
		return wrapErr(err)
	}
	b.ids.Forget(path)
	b.logForget(path)
	return nil
}
