// Command largefiles shows that simplefs handles big data in constant memory:
//
//   - copying a multi-GiB file with simplefs.Copy (1 MiB chunks),
//
//   - streaming with io.NewSectionReader / io.NewOffsetWriter, which turn
//     any FileHandle (io.ReaderAt + io.WriterAt) into a Reader / Writer,
//
//   - listing a 100,000-entry directory page by page with simplefs.Entries,
//
//   - abandoning a copy midway with a context deadline.
//
//     go run ./examples/largefiles               # 2 GiB, in a temp dir
//     go run ./examples/largefiles --size 8GiB
//
// The source file is created sparse (Truncate), so the demo needs little disk
// space on filesystems that support holes.
package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"ella.to/simplefs"
	"ella.to/simplefs/dirfs"
	"ella.to/simplefs/memfs"
)

func main() {
	size := flag.String("size", "2GiB", "size of the file to copy (e.g. 512MiB, 4GiB)")
	entries := flag.Int("entries", 100_000, "files in the big directory")
	flag.Parse()
	n, err := parseSize(*size)
	if err == nil {
		err = run(context.Background(), n, *entries)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "largefiles:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, size int64, entries int) error {
	dir, err := os.MkdirTemp("", "simplefs-largefiles-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	p, err := dirfs.New(dir)
	if err != nil {
		return err
	}
	defer p.Close()

	fmt.Printf("== Copy a %s file\n", human(size))
	src, err := simplefs.Create(ctx, p, "/big.bin", 0)
	if err != nil {
		return err
	}
	if err := src.Truncate(ctx, size); err != nil { // sparse: zeros, no disk used
		return err
	}
	// Stamp a pattern every 64 MiB so the checksum proves real data moved.
	for off := int64(0); off < size; off += 64 << 20 {
		src.WriteAt([]byte(fmt.Sprintf("block at %d", off)), off)
	}
	src.Close()

	before := heapInUse()
	start := time.Now()
	n, err := simplefs.Copy(ctx, p, "/copy.bin", p, "/big.bin")
	if err != nil {
		return err
	}
	took := time.Since(start)
	fmt.Printf("copied %s in %v (%.0f MiB/s); heap grew by %s\n",
		human(n), took.Round(time.Millisecond), float64(n)/(1<<20)/took.Seconds(), human(heapInUse()-before))

	fmt.Println("== Stream through io.NewSectionReader")
	a, err := checksum(ctx, p, "/big.bin")
	if err != nil {
		return err
	}
	b, err := checksum(ctx, p, "/copy.bin")
	if err != nil {
		return err
	}
	fmt.Printf("sha256 equal: %v (%x...)\n", a == b, a[:6])

	fmt.Println("== Abandon a copy with a deadline")
	tctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	n, err = simplefs.Copy(tctx, p, "/partial.bin", p, "/big.bin")
	fmt.Printf("stopped after %s: %v (DeadlineExceeded: %v)\n",
		human(n), err, errors.Is(err, context.DeadlineExceeded))

	fmt.Println("== Write with io.NewOffsetWriter")
	h, err := simplefs.Create(ctx, p, "/numbers.txt", 0)
	if err != nil {
		return err
	}
	w := io.NewOffsetWriter(h, 0) // an io.Writer over WriteAt
	for i := range 100_000 {
		fmt.Fprintln(w, i)
	}
	info, _ := h.Stat(ctx)
	h.Close()
	fmt.Printf("wrote %s of text\n", human(info.Size))

	fmt.Printf("== A directory with %d entries (memfs)\n", entries)
	m := memfs.New()
	m.Mkdir(ctx, "/many", simplefs.MkdirOptions{})
	for i := range entries {
		f, err := m.Open(ctx, "/many/f"+strconv.Itoa(i), simplefs.OpenOptions{Write: true, Create: true})
		if err != nil {
			return err
		}
		f.Close()
	}
	page, _ := m.ReadDir(ctx, "/many", simplefs.Page{Limit: 1000})
	fmt.Printf("one page: %d entries, next token %q\n", len(page.Entries), page.NextToken)
	start = time.Now()
	count := 0
	for _, err := range simplefs.Entries(ctx, m, "/many") { // walks every page
		if err != nil {
			return err
		}
		count++
	}
	fmt.Printf("Entries visited %d in %v\n", count, time.Since(start).Round(time.Millisecond))
	return nil
}

// checksum hashes a file by streaming it: a FileHandle is an io.ReaderAt, so
// io.NewSectionReader turns it into an io.Reader for any standard API.
func checksum(ctx context.Context, p simplefs.FileProvider, path string) ([32]byte, error) {
	h, err := p.Open(ctx, path, simplefs.OpenOptions{})
	if err != nil {
		return [32]byte{}, err
	}
	defer h.Close()
	info, err := h.Stat(ctx)
	if err != nil {
		return [32]byte{}, err
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, io.NewSectionReader(h, 0, info.Size)); err != nil {
		return [32]byte{}, err
	}
	return [32]byte(sum.Sum(nil)), nil
}

func heapInUse() int64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return int64(ms.HeapInuse)
}

func parseSize(s string) (int64, error) {
	units := map[string]int64{"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40, "": 1}
	for suffix, mult := range units {
		if suffix != "" && strings.HasSuffix(s, suffix) {
			n, err := strconv.ParseInt(strings.TrimSuffix(s, suffix), 10, 64)
			return n * mult, err
		}
	}
	return strconv.ParseInt(s, 10, 64)
}

func human(n int64) string {
	switch {
	case n < 0:
		return "-" + human(-n)
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
