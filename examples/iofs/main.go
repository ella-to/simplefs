// Command iofs shows the two bridges between simplefs and the standard
// library's io/fs:
//
//   - simplefs.FS(ctx, p): any provider becomes an fs.FS, so fs.WalkDir,
//     fs.ReadFile, fs.Glob, template.ParseFS and http.FileServerFS work on
//     it. Symlinks are followed, as io/fs expects.
//
//   - simplefs.FromFS(fsys): any fs.FS — an embed.FS, a zip archive,
//     os.DirFS — becomes a read-only provider, so it can be served over NFS
//     or WebDAV, or mounted.
//
//     go run ./examples/iofs              # run the demo
//     go run ./examples/iofs --http :8080 # also serve the memfs tree over HTTP
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"text/template"

	"ella.to/simplefs"
	"ella.to/simplefs/memfs"
)

//go:embed testdata
var embedded embed.FS

func main() {
	httpAddr := flag.String("http", "", "serve the demo tree with http.FileServerFS on this address")
	flag.Parse()
	if err := run(context.Background(), *httpAddr); err != nil {
		fmt.Fprintln(os.Stderr, "iofs:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, httpAddr string) error {
	// A provider with a small tree, a template, and symlinks.
	p := memfs.New()
	simplefs.MkdirAll(ctx, p, "/site/posts", 0)
	simplefs.WriteFile(ctx, p, "/site/posts/first.md", []byte("# First post\n"), 0)
	simplefs.WriteFile(ctx, p, "/site/posts/second.md", []byte("# Second post\n"), 0)
	simplefs.WriteFile(ctx, p, "/site/page.tmpl", []byte("Hello, {{.}}!\n"), 0)
	p.Symlink(ctx, "posts/first.md", "/site/latest.md") // relative target
	p.Symlink(ctx, "/site/posts", "/blog")              // absolute target, a directory

	fsys := simplefs.FS(ctx, p)

	fmt.Println("== fs.WalkDir over a provider")
	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fmt.Printf("  %-22s %v\n", path, d.Type())
		return nil
	}); err != nil {
		return err
	}

	fmt.Println("== following symlinks")
	latest, err := fs.ReadFile(fsys, "site/latest.md")
	if err != nil {
		return err
	}
	fmt.Printf("  site/latest.md -> %q\n", latest)
	posts, err := fs.Glob(fsys, "blog/*.md")
	if err != nil {
		return err
	}
	fmt.Println("  blog/*.md:", posts)

	fmt.Println("== template.ParseFS")
	tmpl, err := template.ParseFS(fsys, "site/*.tmpl")
	if err != nil {
		return err
	}
	tmpl.ExecuteTemplate(os.Stdout, "page.tmpl", "io/fs")

	fmt.Println("== FromFS(embed.FS): a read-only provider")
	ep := simplefs.FromFS(embedded)
	data, err := simplefs.ReadFile(ctx, ep, "/testdata/readme.txt")
	if err != nil {
		return err
	}
	fmt.Printf("  embedded readme: %q\n", data)
	err = simplefs.WriteFile(ctx, ep, "/testdata/new.txt", nil, 0)
	fmt.Println("  write attempt:", err, "| ErrReadOnly:", errors.Is(err, simplefs.ErrReadOnly))

	fmt.Println("== FromFS(zip.Reader)")
	zp, err := zipProvider()
	if err != nil {
		return err
	}
	for e, err := range simplefs.Entries(ctx, zp, "/docs") {
		if err != nil {
			return err
		}
		fmt.Printf("  zip /docs/%s (%d bytes)\n", e.Name, e.Info.Size)
	}
	// Copy out of the archive into the writable provider.
	if _, err := simplefs.Copy(ctx, p, "/from-zip.txt", zp, "/docs/a.txt"); err != nil {
		return err
	}
	fmt.Println("  copied /docs/a.txt out of the zip")

	if httpAddr != "" {
		fmt.Printf("== serving the tree on http://%s/\n", httpAddr)
		return http.ListenAndServe(httpAddr, http.FileServerFS(fsys))
	}
	return nil
}

// zipProvider builds a zip archive in memory and serves it as a provider.
func zipProvider() (simplefs.FileProvider, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{"docs/a.txt": "alpha", "docs/b.txt": "bravo!"} {
		w, err := zw.Create(name)
		if err != nil {
			return nil, err
		}
		w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		return nil, err
	}
	return simplefs.FromFS(zr), nil
}
