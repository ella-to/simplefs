//go:build !darwin || !cgo

// The c-archive entry point only makes sense on macOS, where the File
// Provider extension exists; this stub keeps `go build ./...` working on
// other platforms and without cgo.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "examples/fileprovider/archive: the File Provider host needs macOS and cgo")
	os.Exit(1)
}
