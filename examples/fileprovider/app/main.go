//go:build darwin && cgo

// Command app is the container app for the File Provider extension: the
// executable inside SimpleFS.app that manages the domain from Go via the
// fileprovider package. The OS loads the embedded .appex in a separate process on demand;
// this app does no I/O on the files itself.
//
//	SimpleFS                              mount at the default location
//	SimpleFS --path ~/simplefs            mount and expose it at ~/simplefs
//	SimpleFS --remove [--path ~/simplefs] unmount (and remove the ~/simplefs symlink)
//
// macOS chooses where the domain really lives (~/Library/CloudStorage); with
// --path the fileprovider package symlinks your path to it, so the folder appears
// where you asked. It must run from inside the signed bundle (see `make
// mount`, which passes MOUNT_PATH as --path).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"ella.to/simplefs/fileprovider"
)

const (
	domainID    = "to.ella.simplefs.domain"
	displayName = "SimpleFS"
)

func main() {
	path := flag.String("path", "", "expose the mount at this path (symlinked to the real location)")
	remove := flag.Bool("remove", false, "unmount instead of mounting")
	flag.Parse()
	ctx := context.Background()

	if *remove {
		if err := fileprovider.RemoveDomainAt(ctx, domainID, *path); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("Removed domain.")
		return
	}

	realPath, err := fileprovider.AddDomainAt(ctx, domainID, displayName, *path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	where := realPath
	if *path != "" {
		where = *path
		fmt.Printf("Mounted. Files are at:\n  %s  (-> %s)\n", *path, realPath)
	} else {
		fmt.Printf("Mounted. Files are at:\n  %s\n", realPath)
	}
	fmt.Printf("Try:\n  echo hello > %q && cat %q\n", where+"/hello.txt", where+"/hello.txt")
	fmt.Println("\nIf the folder hangs, enable the extension once:")
	fmt.Println("  System Settings > General > Login Items & Extensions > File Providers > SimpleFS")
}
