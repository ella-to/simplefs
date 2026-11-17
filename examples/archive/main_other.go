//go:build !darwin

// The c-archive entry point only makes sense on macOS, where the File
// Provider extension exists; this stub keeps `go build ./...` working on
// other platforms.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "examples/archive: the File Provider host is macOS-only")
	os.Exit(1)
}
