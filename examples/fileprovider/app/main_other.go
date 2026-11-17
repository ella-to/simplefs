//go:build !darwin || !cgo

// The container app only makes sense on macOS, where the File Provider
// extension exists; this stub keeps `go build ./...` working elsewhere.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "examples/fileprovider/app: the File Provider host needs macOS and cgo")
	os.Exit(1)
}
