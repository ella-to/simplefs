//go:build darwin

// Command archive is the c-archive entry point for mounting a simplefs provider
// on macOS. It registers the in-memory provider through mount.Register and
// blank-imports the capi bridge so its //export symbols are linked into the
// static library the Objective-C File Provider extension calls.
//
// Build (or just run `make mount` at the repository root, which also builds,
// signs, and installs the host app bundle from macos/):
//
//	CGO_ENABLED=1 go build -buildmode=c-archive -o build/libprovider.a ./examples/archive
//
// To mount your own backend, replace memfs.New() with any simplefs.FileProvider.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"ella.to/simplefs"
	"ella.to/simplefs/memfs"
	"ella.to/simplefs/mount"

	// Linked for its //export C symbols (ProviderListJSON, ProviderCreate, ...).
	_ "ella.to/simplefs/capi"
)

func init() {
	provider := memfs.New()
	mount.Register(provider)

	// Demonstrate the WatchProvider capability across a real mount: append
	// every event to <HostDir>/events.log (the extension's sandbox home), so
	// `make events` can tail what Finder does to the in-memory filesystem.
	go logEvents(provider)
}

func logEvents(p simplefs.WatchProvider) {
	events, err := p.Watch(context.Background(), "/", true)
	if err != nil {
		return
	}
	for ev := range events {
		dir := mount.HostDir()
		if dir == "" {
			continue // not running inside the extension yet
		}
		line := fmt.Sprintf("%s  %-6s %s", time.Now().Format(time.RFC3339), ev.Type, ev.Path)
		if ev.OldPath != "" {
			line += " (from " + ev.OldPath + ")"
		}
		if ev.Info != nil {
			line += fmt.Sprintf(" size=%d", ev.Info.Size)
		}
		if f, err := os.OpenFile(filepath.Join(dir, "events.log"),
			os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintln(f, line)
			f.Close()
		}
	}
}

func main() {}
