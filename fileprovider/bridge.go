package fileprovider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

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
// paths are discovered and re-keying them on rename and delete. The table
// lives in memory: after a process restart items receive fresh IDs and the
// system re-syncs from scratch.
type Bridge struct {
	ctx context.Context
	p   simplefs.FileProvider

	ids *idtable.Table[string]
}

// NewBridge wraps p without registering it (useful for tests). Provider calls
// receive context.Background(); the system does not carry a per-request
// context across the extension boundary.
func NewBridge(p simplefs.FileProvider) *Bridge {
	var next uint64
	return &Bridge{
		ctx: context.Background(),
		p:   p,
		ids: idtable.New(RootID, func() string {
			next++
			return "sfs-" + strconv.FormatUint(next, 10)
		}),
	}
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
	Content        []byte
}

// idFor returns the stable ID for path, assigning one on first sight.
func (b *Bridge) idFor(path string) string { return b.ids.ID(path) }

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

// Create adds a new file or folder the user created under parentID.
func (b *Bridge) Create(name, parentID string, dir bool, content []byte) (Item, error) {
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
		if info, err = writeAll(b.ctx, h, content); err != nil {
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
		if _, err := writeAll(b.ctx, h, req.Content); err != nil {
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
	return nil
}

// writeAll writes content at offset 0, syncs, and returns the handle's final
// metadata.
func writeAll(ctx context.Context, h simplefs.FileHandle, content []byte) (simplefs.FileInfo, error) {
	if len(content) > 0 {
		if _, err := h.WriteAt(content, 0); err != nil {
			return simplefs.FileInfo{}, err
		}
	}
	if err := h.Sync(ctx); err != nil {
		return simplefs.FileInfo{}, err
	}
	return h.Stat(ctx)
}
