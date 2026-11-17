// Package providertest is a conformance suite for simplefs.FileProvider
// implementations. It checks the contract documented on
// simplefs.FileProvider, simplefs.FileHandle, and the capability interfaces,
// so a new provider can be validated with one test:
//
//	func TestConformance(t *testing.T) {
//		providertest.Run(t, func(t *testing.T) simplefs.FileProvider {
//			return myfs.New(t.TempDir())
//		}, providertest.Options{})
//	}
//
// Capabilities are detected with simplefs.As; their subtests are skipped
// when the provider lacks the interface or reports ErrNotSupported.
package providertest

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ella.to/simplefs"
)

// Options adapt the suite to a provider's documented limitations.
type Options struct {
	// ReadOnly runs only the read-only checks: every mutation must fail
	// with ErrReadOnly, and whatever the root lists must be readable. The
	// provider may be empty or pre-populated.
	ReadOnly bool

	// CaseInsensitive skips checks that need "a" and "A" to be distinct
	// names (NTFS, default APFS).
	CaseInsensitive bool

	// NormalizesUnicode skips checks that need NFC and NFD spellings to be
	// distinct names (APFS).
	NormalizesUnicode bool

	// MaxNameLength is the longest name the provider accepts, in bytes.
	// Zero means simplefs.MaxNameLength (255).
	MaxNameLength int

	// DirEntries is how many files the pagination check creates. Zero
	// means 10000 (500 with -short).
	DirEntries int
}

// NewFunc returns a fresh, empty provider for one subtest (or, with
// Options.ReadOnly, a fresh read-only provider).
type NewFunc func(t *testing.T) simplefs.FileProvider

// Run runs the conformance suite against providers returned by newProvider.
func Run(t *testing.T, newProvider NewFunc, opts Options) {
	if opts.MaxNameLength == 0 {
		opts.MaxNameLength = simplefs.MaxNameLength
	}
	if opts.DirEntries == 0 {
		opts.DirEntries = 10000
		if testing.Short() {
			opts.DirEntries = 500
		}
	}
	s := &suite{newProvider: newProvider, opts: opts}
	if opts.ReadOnly {
		t.Run("ReadOnly", s.readOnly)
		return
	}
	groups := []struct {
		name string
		fn   func(*testing.T)
	}{
		{"Paths", s.paths},
		{"Names", s.names},
		{"Stat", s.stat},
		{"Open", s.open},
		{"Handle", s.handle},
		{"Metadata", s.metadata},
		{"ReadDir", s.readDir},
		{"Mkdir", s.mkdir},
		{"Remove", s.remove},
		{"Rename", s.rename},
		{"Context", s.context},
		{"Symlink", s.symlink},
		{"HardLink", s.hardLink},
		{"Chmod", s.chmod},
		{"Chtimes", s.chtimes},
		{"Xattr", s.xattr},
		{"StatFS", s.statFS},
		{"Watch", s.watch},
	}
	for _, g := range groups {
		t.Run(g.name, g.fn)
	}
}

type suite struct {
	newProvider NewFunc
	opts        Options
}

// fresh returns a new provider for t.
func (s *suite) fresh(t *testing.T) simplefs.FileProvider {
	t.Helper()
	p := s.newProvider(t)
	if p == nil {
		t.Fatal("providertest: NewFunc returned nil")
	}
	return p
}

// run runs fn as a subtest with its own provider.
func (s *suite) run(t *testing.T, name string, fn func(t *testing.T, ctx context.Context, p simplefs.FileProvider)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		fn(t, t.Context(), s.fresh(t))
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: err = %v, want %v", what, err, want)
	}
}

func write(t *testing.T, ctx context.Context, p simplefs.FileProvider, path, content string) {
	t.Helper()
	must(t, simplefs.WriteFile(ctx, p, path, []byte(content), 0))
}

func read(t *testing.T, ctx context.Context, p simplefs.FileProvider, path string) string {
	t.Helper()
	data, err := simplefs.ReadFile(ctx, p, path)
	must(t, err)
	return string(data)
}

func mkdir(t *testing.T, ctx context.Context, p simplefs.FileProvider, path string) {
	t.Helper()
	must(t, p.Mkdir(ctx, path, simplefs.MkdirOptions{}))
}

func exists(t *testing.T, ctx context.Context, p simplefs.FileProvider, path string) bool {
	t.Helper()
	ok, err := simplefs.Exists(ctx, p, path)
	must(t, err)
	return ok
}

func names(t *testing.T, ctx context.Context, p simplefs.FileProvider, dir string) []string {
	t.Helper()
	var out []string
	for e, err := range simplefs.Entries(ctx, p, dir) {
		must(t, err)
		out = append(out, e.Name)
	}
	slices.Sort(out)
	return out
}

func open(t *testing.T, ctx context.Context, p simplefs.FileProvider, path string, opts simplefs.OpenOptions) simplefs.FileHandle {
	t.Helper()
	h, err := p.Open(ctx, path, opts)
	must(t, err)
	t.Cleanup(func() { h.Close() })
	return h
}

// skipUnsupported skips the test when err reports ErrNotSupported.
func skipUnsupported(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, simplefs.ErrNotSupported) {
		t.Skipf("not supported: %v", err)
	}
}

func (s *suite) paths(t *testing.T) {
	s.run(t, "UncleanInput", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		mkdir(t, ctx, p, "/a")
		write(t, ctx, p, "a//b/../f.txt", "x")
		for _, in := range []string{"/a/f.txt", "a/f.txt", "/a/./f.txt", "//a//f.txt/", "/../a/f.txt"} {
			info, err := p.Stat(ctx, in)
			must(t, err)
			if info.Path != "/a/f.txt" {
				t.Errorf("Stat(%q).Path = %q, want /a/f.txt", in, info.Path)
			}
		}
	})
	s.run(t, "RootIsDir", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		for _, root := range []string{"/", "", "."} {
			info, err := p.Stat(ctx, root)
			must(t, err)
			if !info.IsDir() || info.Path != "/" {
				t.Errorf("Stat(%q) = %+v, want directory /", root, info)
			}
		}
	})
	s.run(t, "RootImmutable", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		wantErr(t, "Remove(/)", p.Remove(ctx, "/", simplefs.RemoveOptions{Recursive: true}), simplefs.ErrInvalid)
		mkdir(t, ctx, p, "/d")
		wantErr(t, "Rename(/)", p.Rename(ctx, "/", "/d/x", simplefs.RenameOptions{}), simplefs.ErrInvalid)
		wantErr(t, "Rename(to /)", p.Rename(ctx, "/d", "/", simplefs.RenameOptions{Overwrite: true}), simplefs.ErrInvalid)
	})
}

func (s *suite) names(t *testing.T) {
	s.run(t, "PreservedBytes", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		list := []string{"with space", ".hidden", "ünïcødé", "日本語", "emoji-😀", "a+b=c", "quote'd"}
		for _, n := range list {
			write(t, ctx, p, "/"+n, n)
		}
		got := names(t, ctx, p, "/")
		want := slices.Clone(list)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("names = %q, want %q", got, want)
		}
		for _, n := range list {
			if c := read(t, ctx, p, "/"+n); c != n {
				t.Errorf("content of %q = %q", n, c)
			}
		}
	})
	s.run(t, "DistinctNormalizations", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		if s.opts.NormalizesUnicode {
			t.Skip("provider normalizes Unicode")
		}
		nfc, nfd := "café", "café"
		write(t, ctx, p, "/"+nfc, "nfc")
		write(t, ctx, p, "/"+nfd, "nfd")
		if read(t, ctx, p, "/"+nfc) != "nfc" || read(t, ctx, p, "/"+nfd) != "nfd" {
			t.Fatal("NFC and NFD names collided")
		}
	})
	s.run(t, "DistinctCase", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		if s.opts.CaseInsensitive {
			t.Skip("provider is case-insensitive")
		}
		write(t, ctx, p, "/a", "lower")
		write(t, ctx, p, "/A", "upper")
		if read(t, ctx, p, "/a") != "lower" || read(t, ctx, p, "/A") != "upper" {
			t.Fatal("a and A collided")
		}
	})
	s.run(t, "Length", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		longest := strings.Repeat("n", s.opts.MaxNameLength)
		write(t, ctx, p, "/"+longest, "ok")
		if read(t, ctx, p, "/"+longest) != "ok" {
			t.Fatal("longest name not readable")
		}
		err := simplefs.WriteFile(ctx, p, "/"+longest+"x", nil, 0)
		if !errors.Is(err, simplefs.ErrNameTooLong) && !errors.Is(err, simplefs.ErrInvalid) {
			t.Fatalf("name over the limit: err = %v, want ErrNameTooLong", err)
		}
	})
}

func (s *suite) stat(t *testing.T) {
	s.run(t, "Missing", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		_, err := p.Stat(ctx, "/missing")
		wantErr(t, "Stat(missing)", err, simplefs.ErrNotExist)
		_, err = p.Stat(ctx, "/missing/child")
		wantErr(t, "Stat(missing/child)", err, simplefs.ErrNotExist)
	})
	s.run(t, "ThroughFile", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		write(t, ctx, p, "/f", "")
		_, err := p.Stat(ctx, "/f/child")
		if !errors.Is(err, simplefs.ErrNotDir) && !errors.Is(err, simplefs.ErrNotExist) {
			t.Fatalf("Stat(file/child) = %v, want ErrNotDir", err)
		}
	})
	s.run(t, "Types", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		write(t, ctx, p, "/f", "abc")
		mkdir(t, ctx, p, "/d")
		fi, err := p.Stat(ctx, "/f")
		must(t, err)
		if !fi.Type.IsFile() || fi.Size != 3 || fi.Name() != "f" {
			t.Errorf("file info = %+v", fi)
		}
		di, err := p.Stat(ctx, "/d")
		must(t, err)
		if !di.IsDir() {
			t.Errorf("dir info = %+v", di)
		}
	})
}

func (s *suite) open(t *testing.T) {
	s.run(t, "DefaultsToRead", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		write(t, ctx, p, "/f", "data")
		h := open(t, ctx, p, "/f", simplefs.OpenOptions{})
		buf := make([]byte, 4)
		if n, err := h.ReadAt(buf, 0); n != 4 || (err != nil && err != io.EOF) {
			t.Fatalf("ReadAt = %d, %v", n, err)
		}
	})
	s.run(t, "FlagsNeedWrite", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		write(t, ctx, p, "/f", "data")
		_, err := p.Open(ctx, "/f", simplefs.OpenOptions{Read: true, Truncate: true})
		wantErr(t, "Truncate without Write", err, simplefs.ErrInvalid)
		_, err = p.Open(ctx, "/f", simplefs.OpenOptions{Read: true, Append: true})
		wantErr(t, "Append without Write", err, simplefs.ErrInvalid)
	})
	s.run(t, "Directory", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		mkdir(t, ctx, p, "/d")
		_, err := p.Open(ctx, "/d", simplefs.OpenOptions{})
		wantErr(t, "Open(dir)", err, simplefs.ErrIsDir)
	})
	s.run(t, "Missing", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		_, err := p.Open(ctx, "/missing", simplefs.OpenOptions{Read: true})
		wantErr(t, "Open(missing)", err, simplefs.ErrNotExist)
		_, err = p.Open(ctx, "/nodir/f", simplefs.OpenOptions{Write: true, Create: true})
		wantErr(t, "Create in missing dir", err, simplefs.ErrNotExist)
	})
	s.run(t, "Exclusive", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		excl := simplefs.OpenOptions{Write: true, Create: true, Exclusive: true}
		h, err := p.Open(ctx, "/f", excl)
		must(t, err)
		h.Close()
		_, err = p.Open(ctx, "/f", excl)
		wantErr(t, "exclusive create of existing file", err, simplefs.ErrExist)
	})
	s.run(t, "CreateMode", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		h, err := p.Open(ctx, "/f", simplefs.OpenOptions{Write: true, Create: true, Mode: 0o600})
		must(t, err)
		h.Close()
		info, err := p.Stat(ctx, "/f")
		must(t, err)
		if info.Mode != 0 && info.Mode.Perm() != 0o600 {
			t.Errorf("mode = %v, want 0600", info.Mode)
		}
		// Mode is ignored for an existing file.
		h, err = p.Open(ctx, "/f", simplefs.OpenOptions{Write: true, Create: true, Mode: 0o644})
		must(t, err)
		h.Close()
		if again, _ := p.Stat(ctx, "/f"); again.Mode != info.Mode {
			t.Errorf("mode changed on reopen: %v -> %v", info.Mode, again.Mode)
		}
	})
	s.run(t, "Truncate", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		write(t, ctx, p, "/f", "data")
		h, err := p.Open(ctx, "/f", simplefs.OpenOptions{Write: true, Truncate: true})
		must(t, err)
		h.Close()
		if c := read(t, ctx, p, "/f"); c != "" {
			t.Fatalf("content after Truncate = %q", c)
		}
	})
}

func (s *suite) handle(t *testing.T) {
	rw := simplefs.OpenOptions{Read: true, Write: true}
	s.run(t, "EOF", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		write(t, ctx, p, "/f", "abc")
		h := open(t, ctx, p, "/f", simplefs.OpenOptions{Read: true})
		buf := make([]byte, 10)
		n, err := h.ReadAt(buf, 1)
		if n != 2 || err != io.EOF || string(buf[:n]) != "bc" {
			t.Errorf("short read = %d, %v, %q; want 2, EOF, bc", n, err, buf[:n])
		}
		if n, err := h.ReadAt(buf, 3); n != 0 || err != io.EOF {
			t.Errorf("read at EOF = %d, %v", n, err)
		}
		if n, err := h.ReadAt(buf, 100); n != 0 || err != io.EOF {
			t.Errorf("read past EOF = %d, %v", n, err)
		}
		if _, err := h.ReadAt(buf, -1); !errors.Is(err, simplefs.ErrInvalid) {
			t.Errorf("negative offset = %v, want ErrInvalid", err)
		}
	})
	s.run(t, "WriteGap", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		h := open(t, ctx, p, "/f", simplefs.OpenOptions{Read: true, Write: true, Create: true})
		_, err := h.WriteAt([]byte("Z"), 5)
		must(t, err)
		buf := make([]byte, 6)
		n, _ := h.ReadAt(buf, 0)
		if string(buf[:n]) != "\x00\x00\x00\x00\x00Z" {
			t.Fatalf("content = %q", buf[:n])
		}
	})
	s.run(t, "WriteNeedsWrite", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		write(t, ctx, p, "/f", "abc")
		h := open(t, ctx, p, "/f", simplefs.OpenOptions{Read: true})
		_, err := h.WriteAt([]byte("x"), 0)
		if !errors.Is(err, simplefs.ErrPermission) && !errors.Is(err, simplefs.ErrInvalid) {
			t.Fatalf("write on read-only handle = %v, want ErrPermission", err)
		}
		if c := read(t, ctx, p, "/f"); c != "abc" {
			t.Fatalf("content changed to %q", c)
		}
	})
	s.run(t, "Append", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		write(t, ctx, p, "/f", "abc")
		h := open(t, ctx, p, "/f", simplefs.OpenOptions{Write: true, Append: true})
		_, err := h.WriteAt([]byte("de"), 0)
		must(t, err)
		h.Close()
		if c := read(t, ctx, p, "/f"); c != "abcde" {
			t.Fatalf("content = %q, want abcde", c)
		}
	})
	s.run(t, "Truncate", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		write(t, ctx, p, "/f", "abcdef")
		h := open(t, ctx, p, "/f", rw)
		must(t, h.Truncate(ctx, 2))
		must(t, h.Truncate(ctx, 4))
		info, err := h.Stat(ctx)
		must(t, err)
		if info.Size != 4 {
			t.Fatalf("size = %d", info.Size)
		}
		h.Close()
		if c := read(t, ctx, p, "/f"); c != "ab\x00\x00" {
			t.Fatalf("content = %q", c)
		}
	})
	s.run(t, "Closed", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		write(t, ctx, p, "/f", "abc")
		h, err := p.Open(ctx, "/f", rw)
		must(t, err)
		must(t, h.Close())
		_, err = h.ReadAt(make([]byte, 1), 0)
		wantErr(t, "ReadAt after Close", err, simplefs.ErrClosed)
		_, err = h.WriteAt([]byte("x"), 0)
		wantErr(t, "WriteAt after Close", err, simplefs.ErrClosed)
		wantErr(t, "second Close", h.Close(), simplefs.ErrClosed)
	})
	s.run(t, "ConcurrentWrites", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		h := open(t, ctx, p, "/f", simplefs.OpenOptions{Read: true, Write: true, Create: true})
		const workers, chunk = 32, 1024
		var wg sync.WaitGroup
		for i := range workers {
			wg.Go(func() {
				buf := slices.Repeat([]byte{byte('A' + i%26)}, chunk)
				if _, err := h.WriteAt(buf, int64(i*chunk)); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		must(t, h.Sync(ctx))
		buf := make([]byte, workers*chunk)
		n, err := h.ReadAt(buf, 0)
		if n != len(buf) || (err != nil && err != io.EOF) {
			t.Fatalf("read back %d, %v", n, err)
		}
		for i := range workers {
			for _, b := range buf[i*chunk : (i+1)*chunk] {
				if b != byte('A'+i%26) {
					t.Fatalf("chunk %d corrupted", i)
				}
			}
		}
	})
	s.run(t, "SurvivesRename", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		mkdir(t, ctx, p, "/a")
		write(t, ctx, p, "/a/f", "abc")
		h := open(t, ctx, p, "/a/f", rw)
		must(t, p.Rename(ctx, "/a", "/b", simplefs.RenameOptions{}))
		_, err := h.WriteAt([]byte("Z"), 0)
		must(t, err)
		must(t, h.Sync(ctx))
		info, err := h.Stat(ctx)
		must(t, err)
		if info.Path != "/b/f" {
			t.Errorf("Stat after rename: Path = %q, want /b/f", info.Path)
		}
		if c := read(t, ctx, p, "/b/f"); c != "Zbc" {
			t.Errorf("content = %q, want Zbc", c)
		}
	})
	s.run(t, "AfterRemove", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		write(t, ctx, p, "/f", "abc")
		h := open(t, ctx, p, "/f", simplefs.OpenOptions{Read: true})
		must(t, p.Remove(ctx, "/f", simplefs.RemoveOptions{}))
		buf := make([]byte, 3)
		n, err := h.ReadAt(buf, 0)
		if err != nil && err != io.EOF && !errors.Is(err, simplefs.ErrNotExist) {
			t.Fatalf("read of removed file = %v, want data or ErrNotExist", err)
		}
		if err == nil && string(buf[:n]) != "abc" {
			t.Fatalf("read of removed file = %q", buf[:n])
		}
	})
}

func (s *suite) metadata(t *testing.T) {
	s.run(t, "SizeTracksWrites", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		h := open(t, ctx, p, "/f", simplefs.OpenOptions{Read: true, Write: true, Create: true})
		for _, step := range []struct {
			off  int64
			data string
			size int64
		}{{0, "abc", 3}, {10, "x", 11}, {1, "y", 11}} {
			_, err := h.WriteAt([]byte(step.data), step.off)
			must(t, err)
			info, err := h.Stat(ctx)
			must(t, err)
			if info.Size != step.size {
				t.Fatalf("handle size = %d, want %d", info.Size, step.size)
			}
		}
		must(t, h.Sync(ctx))
		info, err := p.Stat(ctx, "/f")
		must(t, err)
		if info.Size != 11 {
			t.Fatalf("Stat size = %d, want 11", info.Size)
		}
	})
	s.run(t, "ModTimeAndETag", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		write(t, ctx, p, "/f", "one")
		before, err := p.Stat(ctx, "/f")
		must(t, err)
		time.Sleep(20 * time.Millisecond)
		write(t, ctx, p, "/f", "two")
		after, err := p.Stat(ctx, "/f")
		must(t, err)
		if after.ModTime.Before(before.ModTime) {
			t.Errorf("ModTime went backwards: %v -> %v", before.ModTime, after.ModTime)
		}
		if before.ETag != "" && before.ETag == after.ETag {
			t.Errorf("ETag %q unchanged after a content change", before.ETag)
		}
	})
}

func (s *suite) readDir(t *testing.T) {
	s.run(t, "NotDir", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		write(t, ctx, p, "/f", "")
		_, err := p.ReadDir(ctx, "/f", simplefs.Page{})
		wantErr(t, "ReadDir(file)", err, simplefs.ErrNotDir)
		_, err = p.ReadDir(ctx, "/missing", simplefs.Page{})
		wantErr(t, "ReadDir(missing)", err, simplefs.ErrNotExist)
	})
	s.run(t, "ZeroPageListsAll", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		mkdir(t, ctx, p, "/d")
		write(t, ctx, p, "/f", "")
		mkdir(t, ctx, p, "/d/nested")
		page, err := p.ReadDir(ctx, "/", simplefs.Page{})
		must(t, err)
		if page.NextToken != "" {
			t.Errorf("zero Page returned NextToken %q", page.NextToken)
		}
		var got []string
		for _, e := range page.Entries {
			got = append(got, e.Name)
			if e.Name == "d" && !e.IsDir() {
				t.Errorf("entry d has type %v", e.Type)
			}
			if e.Info != nil && e.Info.Path != "/"+e.Name {
				t.Errorf("entry %s Info.Path = %q", e.Name, e.Info.Path)
			}
		}
		slices.Sort(got)
		if !slices.Equal(got, []string{"d", "f"}) {
			t.Fatalf("entries = %v, want [d f] (no . or ..)", got)
		}
	})
	s.run(t, "Pagination", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		mkdir(t, ctx, p, "/d")
		want := make([]string, s.opts.DirEntries)
		for i := range want {
			want[i] = "f" + strconv.Itoa(i)
			h, err := p.Open(ctx, "/d/"+want[i], simplefs.OpenOptions{Write: true, Create: true})
			must(t, err)
			h.Close()
		}
		slices.Sort(want)
		for _, limit := range []int{1, 7, 1000} {
			var got []string
			token := ""
			for pages := 0; ; pages++ {
				if pages > len(want)+1 {
					t.Fatalf("limit %d: pagination does not terminate", limit)
				}
				page, err := p.ReadDir(ctx, "/d", simplefs.Page{Limit: limit, Token: token})
				must(t, err)
				if len(page.Entries) > limit {
					t.Fatalf("limit %d: page has %d entries", limit, len(page.Entries))
				}
				for _, e := range page.Entries {
					got = append(got, e.Name)
				}
				if page.NextToken == "" {
					break
				}
				token = page.NextToken
			}
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Fatalf("limit %d: got %d entries (with duplicates or gaps), want %d", limit, len(got), len(want))
			}
			if limit == 1 && len(want) > 50 {
				break // the other limits add little and cost a lot
			}
		}
	})
	s.run(t, "MutationBetweenPages", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		for i := range 20 {
			write(t, ctx, p, "/f"+strconv.Itoa(i), "")
		}
		page, err := p.ReadDir(ctx, "/", simplefs.Page{Limit: 5})
		must(t, err)
		for _, e := range page.Entries {
			must(t, p.Remove(ctx, "/"+e.Name, simplefs.RemoveOptions{}))
		}
		write(t, ctx, p, "/new", "")
		token := page.NextToken
		for pages := 0; token != ""; pages++ {
			if pages > 30 {
				t.Fatal("pagination loops after a mutation")
			}
			page, err := p.ReadDir(ctx, "/", simplefs.Page{Limit: 5, Token: token})
			if err != nil {
				return // an error is an acceptable answer to a stale token
			}
			token = page.NextToken
		}
	})
}

func (s *suite) mkdir(t *testing.T) {
	s.run(t, "Errors", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		mkdir(t, ctx, p, "/d")
		wantErr(t, "Mkdir(existing)", p.Mkdir(ctx, "/d", simplefs.MkdirOptions{}), simplefs.ErrExist)
		wantErr(t, "Mkdir(missing parent)", p.Mkdir(ctx, "/x/y", simplefs.MkdirOptions{}), simplefs.ErrNotExist)
		write(t, ctx, p, "/f", "")
		wantErr(t, "Mkdir(existing file)", p.Mkdir(ctx, "/f", simplefs.MkdirOptions{}), simplefs.ErrExist)
		err := p.Mkdir(ctx, "/f/sub", simplefs.MkdirOptions{})
		if !errors.Is(err, simplefs.ErrNotDir) && !errors.Is(err, simplefs.ErrNotExist) {
			t.Fatalf("Mkdir under a file = %v, want ErrNotDir", err)
		}
	})
	s.run(t, "Mode", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		must(t, p.Mkdir(ctx, "/d", simplefs.MkdirOptions{Mode: 0o700}))
		info, err := p.Stat(ctx, "/d")
		must(t, err)
		if info.Mode != 0 && info.Mode.Perm() != 0o700 {
			t.Errorf("mode = %v, want 0700", info.Mode)
		}
	})
}

func (s *suite) remove(t *testing.T) {
	s.run(t, "Missing", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		wantErr(t, "Remove(missing)", p.Remove(ctx, "/missing", simplefs.RemoveOptions{}), simplefs.ErrNotExist)
		must(t, p.Remove(ctx, "/missing", simplefs.RemoveOptions{Force: true}))
	})
	s.run(t, "FileAndEmptyDir", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		write(t, ctx, p, "/f", "")
		mkdir(t, ctx, p, "/d")
		must(t, p.Remove(ctx, "/f", simplefs.RemoveOptions{}))
		must(t, p.Remove(ctx, "/d", simplefs.RemoveOptions{}))
		if exists(t, ctx, p, "/f") || exists(t, ctx, p, "/d") {
			t.Fatal("removed paths still exist")
		}
	})
	s.run(t, "NonEmpty", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		mkdir(t, ctx, p, "/d")
		mkdir(t, ctx, p, "/d/sub")
		write(t, ctx, p, "/d/sub/f", "")
		wantErr(t, "Remove(non-empty)", p.Remove(ctx, "/d", simplefs.RemoveOptions{}), simplefs.ErrNotEmpty)
		err := p.Remove(ctx, "/d", simplefs.RemoveOptions{Recursive: true})
		if errors.Is(err, simplefs.ErrNotSupported) {
			err = simplefs.RemoveAll(ctx, p, "/d")
		}
		must(t, err)
		if exists(t, ctx, p, "/d") {
			t.Fatal("/d still exists after recursive remove")
		}
	})
}

func (s *suite) rename(t *testing.T) {
	s.run(t, "Basics", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		write(t, ctx, p, "/a", "A")
		must(t, p.Rename(ctx, "/a", "/a", simplefs.RenameOptions{}))
		must(t, p.Rename(ctx, "/a", "/b", simplefs.RenameOptions{}))
		if exists(t, ctx, p, "/a") || read(t, ctx, p, "/b") != "A" {
			t.Fatal("rename did not move the file")
		}
		wantErr(t, "Rename(missing)", p.Rename(ctx, "/missing", "/x", simplefs.RenameOptions{}), simplefs.ErrNotExist)
		wantErr(t, "Rename(into missing dir)", p.Rename(ctx, "/b", "/nodir/b", simplefs.RenameOptions{}), simplefs.ErrNotExist)
	})
	s.run(t, "Overwrite", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		write(t, ctx, p, "/a", "A")
		write(t, ctx, p, "/b", "B")
		wantErr(t, "Rename onto existing", p.Rename(ctx, "/a", "/b", simplefs.RenameOptions{}), simplefs.ErrExist)
		must(t, p.Rename(ctx, "/a", "/b", simplefs.RenameOptions{Overwrite: true}))
		if read(t, ctx, p, "/b") != "A" || exists(t, ctx, p, "/a") {
			t.Fatal("overwrite did not replace /b")
		}
	})
	s.run(t, "TypeConflicts", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		write(t, ctx, p, "/f", "")
		mkdir(t, ctx, p, "/d")
		mkdir(t, ctx, p, "/full")
		write(t, ctx, p, "/full/x", "")
		mkdir(t, ctx, p, "/empty")
		ow := simplefs.RenameOptions{Overwrite: true}
		wantErr(t, "file over dir", p.Rename(ctx, "/f", "/d", ow), simplefs.ErrIsDir)
		wantErr(t, "dir over file", p.Rename(ctx, "/d", "/f", ow), simplefs.ErrNotDir)
		wantErr(t, "dir over non-empty dir", p.Rename(ctx, "/d", "/full", ow), simplefs.ErrNotEmpty)
		must(t, p.Rename(ctx, "/d", "/empty", ow))
	})
	s.run(t, "IntoItself", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		mkdir(t, ctx, p, "/d")
		mkdir(t, ctx, p, "/d/sub")
		wantErr(t, "dir into itself", p.Rename(ctx, "/d", "/d/sub/d", simplefs.RenameOptions{}), simplefs.ErrInvalid)
	})
	s.run(t, "KeepsSubtree", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		must(t, simplefs.MkdirAll(ctx, p, "/a/b/c", 0))
		write(t, ctx, p, "/a/b/c/f", "deep")
		mkdir(t, ctx, p, "/z")
		must(t, p.Rename(ctx, "/a", "/z/moved", simplefs.RenameOptions{}))
		if read(t, ctx, p, "/z/moved/b/c/f") != "deep" {
			t.Fatal("subtree lost in rename")
		}
		info, err := p.Stat(ctx, "/z/moved/b/c/f")
		must(t, err)
		if info.Path != "/z/moved/b/c/f" {
			t.Fatalf("Path after rename = %q", info.Path)
		}
	})
	s.run(t, "AtomicOverwrite", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		write(t, ctx, p, "/target", "old")
		var wg sync.WaitGroup
		stop := make(chan struct{})
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				data, err := simplefs.ReadFile(ctx, p, "/target")
				if errors.Is(err, simplefs.ErrNotExist) {
					t.Error("target missing during overwrite")
					return
				}
				if err == nil && string(data) != "old" && string(data) != "new" {
					t.Errorf("target content %q", data)
					return
				}
			}
		})
		for range 50 {
			write(t, ctx, p, "/tmp", "new")
			must(t, p.Rename(ctx, "/tmp", "/target", simplefs.RenameOptions{Overwrite: true}))
		}
		close(stop)
		wg.Wait()
	})
}

func (s *suite) context(t *testing.T) {
	s.run(t, "Cancelled", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		write(t, ctx, p, "/f", "x")
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		checks := map[string]error{}
		_, checks["Stat"] = p.Stat(cctx, "/f")
		_, checks["Open"] = p.Open(cctx, "/f", simplefs.OpenOptions{})
		_, checks["ReadDir"] = p.ReadDir(cctx, "/", simplefs.Page{})
		checks["Mkdir"] = p.Mkdir(cctx, "/d", simplefs.MkdirOptions{})
		checks["Remove"] = p.Remove(cctx, "/f", simplefs.RemoveOptions{})
		checks["Rename"] = p.Rename(cctx, "/f", "/g", simplefs.RenameOptions{})
		for op, err := range checks {
			if !errors.Is(err, context.Canceled) {
				t.Errorf("%s with cancelled ctx = %v, want context.Canceled", op, err)
			}
		}
	})
}

func (s *suite) symlink(t *testing.T) {
	s.run(t, "RoundTrip", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		sp, ok := simplefs.As[simplefs.SymlinkProvider](p)
		if !ok {
			t.Skip("no SymlinkProvider")
		}
		write(t, ctx, p, "/target", "t")
		err := sp.Symlink(ctx, "target", "/link")
		skipUnsupported(t, err)
		must(t, err)
		got, err := sp.Readlink(ctx, "/link")
		must(t, err)
		if got != "target" {
			t.Errorf("Readlink = %q", got)
		}
		info, err := p.Stat(ctx, "/link")
		must(t, err)
		if !info.Type.IsSymlink() {
			t.Errorf("Stat(link) type = %v, want symlink (Stat must not follow)", info.Type)
		}
		must(t, sp.Symlink(ctx, "/nowhere", "/dangling"))
		if got, err := sp.Readlink(ctx, "/dangling"); err != nil || got != "/nowhere" {
			t.Errorf("dangling Readlink = %q, %v", got, err)
		}
		_, err = sp.Readlink(ctx, "/target")
		wantErr(t, "Readlink(file)", err, simplefs.ErrInvalid)
		wantErr(t, "Symlink over existing", sp.Symlink(ctx, "x", "/target"), simplefs.ErrExist)
		must(t, p.Remove(ctx, "/link", simplefs.RemoveOptions{}))
		if read(t, ctx, p, "/target") != "t" {
			t.Error("removing a link removed its target")
		}
	})
}

func (s *suite) hardLink(t *testing.T) {
	s.run(t, "SharesContent", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		hp, ok := simplefs.As[simplefs.HardLinkProvider](p)
		if !ok {
			t.Skip("no HardLinkProvider")
		}
		write(t, ctx, p, "/a", "one")
		err := hp.Link(ctx, "/a", "/b")
		skipUnsupported(t, err)
		must(t, err)
		write(t, ctx, p, "/a", "two")
		if read(t, ctx, p, "/b") != "two" {
			t.Error("hard link does not share content")
		}
		must(t, p.Remove(ctx, "/a", simplefs.RemoveOptions{}))
		if read(t, ctx, p, "/b") != "two" {
			t.Error("content lost when the original name was removed")
		}
		mkdir(t, ctx, p, "/d")
		err = hp.Link(ctx, "/d", "/d2")
		if !errors.Is(err, simplefs.ErrIsDir) && !errors.Is(err, simplefs.ErrPermission) {
			t.Errorf("Link(dir) = %v, want ErrIsDir", err)
		}
		wantErr(t, "Link onto existing", hp.Link(ctx, "/b", "/d"), simplefs.ErrExist)
	})
}

func (s *suite) chmod(t *testing.T) {
	s.run(t, "Reflected", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		cp, ok := simplefs.As[simplefs.ChmodProvider](p)
		if !ok {
			t.Skip("no ChmodProvider")
		}
		write(t, ctx, p, "/f", "")
		err := cp.Chmod(ctx, "/f", 0o640)
		skipUnsupported(t, err)
		must(t, err)
		info, err := p.Stat(ctx, "/f")
		must(t, err)
		if info.Mode.Perm() != 0o640 {
			t.Errorf("mode = %v, want 0640", info.Mode)
		}
		wantErr(t, "Chmod(missing)", cp.Chmod(ctx, "/missing", 0o600), simplefs.ErrNotExist)
	})
}

func (s *suite) chtimes(t *testing.T) {
	s.run(t, "Reflected", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		cp, ok := simplefs.As[simplefs.ChtimesProvider](p)
		if !ok {
			t.Skip("no ChtimesProvider")
		}
		write(t, ctx, p, "/f", "")
		want := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
		err := cp.Chtimes(ctx, "/f", want, want)
		skipUnsupported(t, err)
		must(t, err)
		info, err := p.Stat(ctx, "/f")
		must(t, err)
		if !info.ModTime.Equal(want) {
			t.Errorf("ModTime = %v, want %v", info.ModTime, want)
		}
		// A zero time leaves the timestamp unchanged.
		must(t, cp.Chtimes(ctx, "/f", time.Now(), time.Time{}))
		info, err = p.Stat(ctx, "/f")
		must(t, err)
		if !info.ModTime.Equal(want) {
			t.Errorf("zero mtime changed ModTime to %v", info.ModTime)
		}
	})
}

func (s *suite) xattr(t *testing.T) {
	s.run(t, "CRUD", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		xp, ok := simplefs.As[simplefs.XattrProvider](p)
		if !ok {
			t.Skip("no XattrProvider")
		}
		write(t, ctx, p, "/f", "")
		err := xp.SetXattr(ctx, "/f", "user.b", []byte("2"))
		skipUnsupported(t, err)
		must(t, err)
		must(t, xp.SetXattr(ctx, "/f", "user.a", []byte("1")))
		v, err := xp.GetXattr(ctx, "/f", "user.b")
		must(t, err)
		if string(v) != "2" {
			t.Errorf("GetXattr = %q", v)
		}
		list, err := xp.ListXattr(ctx, "/f")
		must(t, err)
		slices.Sort(list)
		if !slices.Equal(list, []string{"user.a", "user.b"}) {
			t.Errorf("ListXattr = %v", list)
		}
		must(t, xp.RemoveXattr(ctx, "/f", "user.a"))
		_, err = xp.GetXattr(ctx, "/f", "user.a")
		wantErr(t, "GetXattr(removed)", err, simplefs.ErrNotExist)
		wantErr(t, "RemoveXattr(missing)", xp.RemoveXattr(ctx, "/f", "user.none"), simplefs.ErrNotExist)
	})
}

func (s *suite) statFS(t *testing.T) {
	s.run(t, "Sane", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		sp, ok := simplefs.As[simplefs.StatFSProvider](p)
		if !ok {
			t.Skip("no StatFSProvider")
		}
		info, err := sp.StatFS(ctx)
		skipUnsupported(t, err)
		must(t, err)
		if info.Available > info.Free || info.Free > info.Blocks {
			t.Errorf("StatFS = %+v; want Available <= Free <= Blocks", info)
		}
		if info.ReadOnly {
			t.Error("writable provider reports ReadOnly")
		}
	})
}

func (s *suite) watch(t *testing.T) {
	collect := func(t *testing.T, events <-chan simplefs.Event, n int) []simplefs.Event {
		t.Helper()
		var got []simplefs.Event
		timeout := time.After(5 * time.Second)
		for len(got) < n {
			select {
			case ev, ok := <-events:
				if !ok {
					t.Fatalf("channel closed after %d events", len(got))
				}
				got = append(got, ev)
			case <-timeout:
				t.Fatalf("got %d events, want %d: %+v", len(got), n, got)
			}
		}
		return got
	}
	s.run(t, "Events", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		wp, ok := simplefs.As[simplefs.WatchProvider](p)
		if !ok {
			t.Skip("no WatchProvider")
		}
		wctx, cancel := context.WithCancel(ctx)
		events, err := wp.Watch(wctx, "/", true)
		skipUnsupported(t, err)
		must(t, err)

		mkdir(t, ctx, p, "/d")
		h, err := p.Open(ctx, "/d/f", simplefs.OpenOptions{Write: true, Create: true})
		must(t, err)
		_, err = h.WriteAt([]byte("x"), 0)
		must(t, err)
		must(t, h.Close())
		must(t, p.Rename(ctx, "/d/f", "/d/g", simplefs.RenameOptions{}))
		must(t, p.Remove(ctx, "/d/g", simplefs.RemoveOptions{}))

		got := collect(t, events, 5)
		want := []struct {
			typ       simplefs.EventType
			path, old string
		}{
			{simplefs.EventCreate, "/d", ""},
			{simplefs.EventCreate, "/d/f", ""},
			{simplefs.EventModify, "/d/f", ""},
			{simplefs.EventRename, "/d/g", "/d/f"},
			{simplefs.EventDelete, "/d/g", ""},
		}
		for i, w := range want {
			if got[i].Type != w.typ || got[i].Path != w.path || got[i].OldPath != w.old {
				t.Errorf("event %d = %v %s (from %q), want %v %s (from %q)",
					i, got[i].Type, got[i].Path, got[i].OldPath, w.typ, w.path, w.old)
			}
		}
		cancel()
		for range events {
		}
	})
	s.run(t, "Scope", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		wp, ok := simplefs.As[simplefs.WatchProvider](p)
		if !ok {
			t.Skip("no WatchProvider")
		}
		must(t, simplefs.MkdirAll(ctx, p, "/w/deep", 0))
		wctx, cancel := context.WithCancel(ctx)
		defer cancel()
		events, err := wp.Watch(wctx, "/w", false)
		skipUnsupported(t, err)
		must(t, err)
		write(t, ctx, p, "/w/deep/hidden", "")
		write(t, ctx, p, "/outside", "")
		write(t, ctx, p, "/w/direct", "")
		got := collect(t, events, 1)
		if got[0].Path != "/w/direct" {
			t.Fatalf("first event %+v; non-recursive watch must only see direct children", got[0])
		}
	})
	s.run(t, "NeverBlocks", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		wp, ok := simplefs.As[simplefs.WatchProvider](p)
		if !ok {
			t.Skip("no WatchProvider")
		}
		wctx, cancel := context.WithCancel(ctx)
		defer cancel()
		_, err := wp.Watch(wctx, "/", true) // never read
		skipUnsupported(t, err)
		must(t, err)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for i := range 1000 {
				write(t, ctx, p, "/f"+strconv.Itoa(i%10), "x")
			}
		}()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatal("writes blocked on an unread watch channel")
		}
	})
	s.run(t, "ClosesOnCancel", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		wp, ok := simplefs.As[simplefs.WatchProvider](p)
		if !ok {
			t.Skip("no WatchProvider")
		}
		wctx, cancel := context.WithCancel(ctx)
		events, err := wp.Watch(wctx, "/", true)
		skipUnsupported(t, err)
		must(t, err)
		cancel()
		timeout := time.After(5 * time.Second)
		for {
			select {
			case _, ok := <-events:
				if !ok {
					return
				}
			case <-timeout:
				t.Fatal("channel not closed after cancel")
			}
		}
	})
}

func (s *suite) readOnly(t *testing.T) {
	s.run(t, "MutationsFail", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		checks := map[string]error{
			"create":   simplefs.WriteFile(ctx, p, "/providertest-new", nil, 0),
			"mkdir":    p.Mkdir(ctx, "/providertest-dir", simplefs.MkdirOptions{}),
			"remove":   p.Remove(ctx, "/providertest-new", simplefs.RemoveOptions{Force: true}),
			"rename":   p.Rename(ctx, "/providertest-a", "/providertest-b", simplefs.RenameOptions{}),
			"truncate": func() error { _, err := p.Open(ctx, "/x", simplefs.OpenOptions{Write: true, Truncate: true}); return err }(),
		}
		for op, err := range checks {
			if !errors.Is(err, simplefs.ErrReadOnly) {
				t.Errorf("%s = %v, want ErrReadOnly", op, err)
			}
		}
		if sp, ok := simplefs.As[simplefs.StatFSProvider](p); ok {
			if info, err := sp.StatFS(ctx); err == nil && !info.ReadOnly {
				t.Error("StatFS does not report ReadOnly")
			}
		}
	})
	s.run(t, "TreeReadable", func(t *testing.T, ctx context.Context, p simplefs.FileProvider) {
		root, err := p.Stat(ctx, "/")
		must(t, err)
		if !root.IsDir() {
			t.Fatal("root is not a directory")
		}
		err = simplefs.WalkDir(ctx, p, "/", func(path string, d simplefs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.Type.IsFile() {
				return nil
			}
			h, err := p.Open(ctx, path, simplefs.OpenOptions{Read: true})
			if err != nil {
				return err
			}
			defer h.Close()
			if _, err := h.WriteAt([]byte("x"), 0); !errors.Is(err, simplefs.ErrReadOnly) && !errors.Is(err, simplefs.ErrPermission) {
				t.Errorf("WriteAt on %s = %v, want ErrReadOnly", path, err)
			}
			data, err := simplefs.ReadFile(ctx, p, path)
			if err != nil {
				return err
			}
			info, err := p.Stat(ctx, path)
			if err != nil {
				return err
			}
			if int64(len(data)) != info.Size {
				t.Errorf("%s: read %d bytes, Stat.Size = %d", path, len(data), info.Size)
			}
			return nil
		})
		if errors.Is(err, fs.SkipAll) {
			err = nil
		}
		must(t, err)
	})
}
