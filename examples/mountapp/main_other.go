//go:build !darwin

// The container app only makes sense on macOS, where the File Provider
// extension exists; this stub keeps `go build ./...` working elsewhere.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "examples/mountapp: the File Provider host is macOS-only")
	os.Exit(1)
}
