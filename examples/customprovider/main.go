// Command customprovider shows the smallest useful provider written from
// scratch: read-only virtual files generated on read (/time.txt and
// /env/<NAME>), browsed through io/fs and, optionally, mounted.
//
//	go run ./examples/customprovider                       # print the tree
//	go run ./examples/customprovider --mount ~/sysfs       # mount it; cat ~/sysfs/time.txt
//	go test ./examples/customprovider                      # providertest, read-only mode
//
// See provider.go: six methods, errors wrapped in *simplefs.PathError,
// ErrReadOnly for every mutation, sorted names as pagination tokens.
package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"

	"ella.to/simplefs"
	"ella.to/simplefs/mount"
)

func main() {
	mountPath := flag.String("mount", "", "mount the provider at this path until Ctrl-C")
	flag.Parse()
	if err := run(*mountPath); err != nil {
		fmt.Fprintln(os.Stderr, "customprovider:", err)
		os.Exit(1)
	}
}

func run(mountPath string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	p := newSysFS()

	// Any provider is an io/fs.FS: the standard library does the walking.
	fsys := simplefs.FS(ctx, p)
	t, err := fs.ReadFile(fsys, "time.txt")
	if err != nil {
		return err
	}
	fmt.Print("time.txt: ", string(t))
	entries, err := fs.ReadDir(fsys, "env")
	if err != nil {
		return err
	}
	fmt.Printf("env/ has %d variables, e.g.:\n", len(entries))
	for _, e := range entries[:min(3, len(entries))] {
		v, _ := fs.ReadFile(fsys, "env/"+e.Name())
		fmt.Printf("  %s = %q\n", e.Name(), v)
	}
	if err := p.Mkdir(ctx, "/new", simplefs.MkdirOptions{}); err != nil {
		fmt.Println("Mkdir:", err) // read-only file system
	}

	if mountPath == "" {
		return nil
	}
	m, err := mount.Mount(ctx, p, mount.Options{Path: mountPath, Name: "sysfs", ReadOnly: true})
	if err != nil {
		return err
	}
	fmt.Printf("mounted at %s — try: cat %s/time.txt; Ctrl-C to unmount\n", m.Path(), m.Path())
	<-ctx.Done()
	return m.Close()
}
