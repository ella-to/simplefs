package simplefs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"iter"
	"math"
)

// Create creates the file at path, or truncates it if it exists, and opens
// it for reading and writing — the equivalent of os.Create. A zero mode
// means 0o644.
func Create(ctx context.Context, p FileProvider, path string, mode FileMode) (FileHandle, error) {
	return p.Open(ctx, path, OpenOptions{Read: true, Write: true, Create: true, Truncate: true, Mode: mode})
}

// ReadFile returns the whole content of the file at path.
func ReadFile(ctx context.Context, p FileProvider, path string) ([]byte, error) {
	h, err := p.Open(ctx, path, OpenOptions{Read: true})
	if err != nil {
		return nil, err
	}
	defer h.Close()

	var buf bytes.Buffer
	if info, err := h.Stat(ctx); err == nil && info.Size > 0 && info.Size < math.MaxInt32 {
		buf.Grow(int(info.Size) + 1) // +1 so ReadFrom sees EOF without growing
	}
	if _, err := buf.ReadFrom(io.NewSectionReader(h, 0, math.MaxInt64)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// WriteFile writes data to the file at path, creating it with mode (0o644
// when zero) or truncating it first — the equivalent of os.WriteFile. The
// data is synced before the file is closed.
func WriteFile(ctx context.Context, p FileProvider, path string, data []byte, mode FileMode) error {
	h, err := Create(ctx, p, path, mode)
	if err != nil {
		return err
	}
	if len(data) > 0 {
		if _, err := h.WriteAt(data, 0); err != nil {
			h.Close()
			return err
		}
	}
	if err := h.Sync(ctx); err != nil {
		h.Close()
		return err
	}
	return h.Close()
}

// Exists reports whether path exists. Only a non-existence error is turned
// into false; any other error is returned.
func Exists(ctx context.Context, p FileProvider, path string) (bool, error) {
	_, err := p.Stat(ctx, path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// MkdirAll creates the directory at path and any missing parents, each with
// mode (0o755 when zero) — the equivalent of os.MkdirAll. It succeeds if
// path already is a directory and fails with ErrNotDir if a component is a
// file.
func MkdirAll(ctx context.Context, p FileProvider, path string, mode FileMode) error {
	path = CleanPath(path)
	info, err := p.Stat(ctx, path)
	if err == nil {
		if info.IsDir() {
			return nil
		}
		return &PathError{Op: "mkdir", Path: path, Err: ErrNotDir}
	}
	if !errors.Is(err, ErrNotExist) {
		return err
	}
	if parent := ParentPath(path); parent != path {
		if err := MkdirAll(ctx, p, parent, mode); err != nil {
			return err
		}
	}
	err = p.Mkdir(ctx, path, MkdirOptions{Mode: mode})
	if errors.Is(err, ErrExist) {
		// Lost a race with a concurrent creator; fine if it made a directory.
		if info, serr := p.Stat(ctx, path); serr == nil && info.IsDir() {
			return nil
		}
	}
	return err
}

// RemoveAll removes path and everything below it — the equivalent of
// os.RemoveAll. A missing path is not an error. Providers that do not
// support recursive Remove (ErrNotSupported) are handled by removing the
// tree bottom-up.
func RemoveAll(ctx context.Context, p FileProvider, path string) error {
	err := p.Remove(ctx, path, RemoveOptions{Recursive: true, Force: true})
	if err == nil || errors.Is(err, ErrNotExist) {
		return nil
	}
	if !errors.Is(err, ErrNotSupported) {
		return err
	}
	return removeTree(ctx, p, CleanPath(path))
}

func removeTree(ctx context.Context, p FileProvider, path string) error {
	info, err := p.Stat(ctx, path)
	if errors.Is(err, ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		// Collect first: removing while paging could skip entries.
		var names []string
		for e, err := range Entries(ctx, p, path) {
			if err != nil {
				return err
			}
			names = append(names, e.Name)
		}
		for _, name := range names {
			if err := removeTree(ctx, p, JoinPath(path, name)); err != nil {
				return err
			}
		}
	}
	err = p.Remove(ctx, path, RemoveOptions{})
	if errors.Is(err, ErrNotExist) {
		return nil
	}
	return err
}

// entriesPageSize is how many entries Entries requests per ReadDir call.
const entriesPageSize = 512

// Entries iterates over every entry of the directory at path, following
// ReadDir pagination transparently. On failure it yields one final
// (zero DirEntry, error) pair and stops.
//
//	for e, err := range simplefs.Entries(ctx, p, "/docs") {
//		if err != nil {
//			return err
//		}
//		fmt.Println(e.Name)
//	}
func Entries(ctx context.Context, p FileProvider, path string) iter.Seq2[DirEntry, error] {
	return func(yield func(DirEntry, error) bool) {
		token := ""
		for {
			if err := ctx.Err(); err != nil {
				yield(DirEntry{}, err)
				return
			}
			page, err := p.ReadDir(ctx, path, Page{Limit: entriesPageSize, Token: token})
			if err != nil {
				yield(DirEntry{}, err)
				return
			}
			for _, e := range page.Entries {
				if !yield(e, nil) {
					return
				}
			}
			if page.NextToken == "" || page.NextToken == token {
				return
			}
			token = page.NextToken
		}
	}
}

// WalkDirFunc is called by WalkDir for every visited path, with the same
// contract as io/fs.WalkDirFunc: return fs.SkipDir to skip a directory's
// contents (or the rest of the current directory, when d is a file),
// fs.SkipAll to stop the walk, or any other error to abort with it.
type WalkDirFunc func(path string, d DirEntry, err error) error

// WalkDir walks the tree rooted at root in lexical order within each
// directory listing, calling fn for every file and directory, root
// included. Symlinks are reported, not followed.
func WalkDir(ctx context.Context, p FileProvider, root string, fn WalkDirFunc) error {
	root = CleanPath(root)
	info, err := p.Stat(ctx, root)
	var d DirEntry
	if err == nil {
		d = DirEntry{Name: info.Name(), Type: info.Type, Info: &info}
	}
	if err != nil {
		err = fn(root, d, err)
	} else {
		err = walkDir(ctx, p, root, d, fn)
	}
	if errors.Is(err, fs.SkipDir) || errors.Is(err, fs.SkipAll) {
		return nil
	}
	return err
}

func walkDir(ctx context.Context, p FileProvider, path string, d DirEntry, fn WalkDirFunc) error {
	if err := fn(path, d, nil); err != nil || !d.IsDir() {
		if errors.Is(err, fs.SkipDir) && d.IsDir() {
			err = nil
		}
		return err
	}
	var children []DirEntry
	for e, err := range Entries(ctx, p, path) {
		if err != nil {
			// Second call for the directory, reporting the listing error.
			if err := fn(path, d, err); err != nil {
				if errors.Is(err, fs.SkipDir) {
					return nil
				}
				return err
			}
			break
		}
		children = append(children, e)
	}
	for _, c := range children {
		if err := walkDir(ctx, p, JoinPath(path, c.Name), c, fn); err != nil {
			if errors.Is(err, fs.SkipDir) {
				break
			}
			return err
		}
	}
	return nil
}

// copyBufSize is the chunk size Copy moves per ReadAt/WriteAt.
const copyBufSize = 1 << 20

// Copy copies the regular file srcPath of src to dstPath of dst (which may
// be the same provider), creating or truncating the destination with the
// source's permission bits. It streams in 1 MiB chunks, so memory use is
// constant, checks ctx between chunks, and returns the bytes copied.
func Copy(ctx context.Context, dst FileProvider, dstPath string, src FileProvider, srcPath string) (int64, error) {
	in, err := src.Open(ctx, srcPath, OpenOptions{Read: true})
	if err != nil {
		return 0, err
	}
	defer in.Close()
	info, err := in.Stat(ctx)
	if err != nil {
		return 0, err
	}
	if info.IsDir() {
		return 0, &PathError{Op: "copy", Path: srcPath, Err: ErrIsDir}
	}
	out, err := Create(ctx, dst, dstPath, info.Mode)
	if err != nil {
		return 0, err
	}

	buf := make([]byte, copyBufSize)
	var off int64
	for {
		if err := ctx.Err(); err != nil {
			out.Close()
			return off, err
		}
		n, rerr := in.ReadAt(buf, off)
		if n > 0 {
			if _, werr := out.WriteAt(buf[:n], off); werr != nil {
				out.Close()
				return off, werr
			}
			off += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			return off, rerr
		}
	}
	if err := out.Sync(ctx); err != nil {
		out.Close()
		return off, err
	}
	return off, out.Close()
}
