// Command watch tails a provider's change events (WatchProvider) while the
// provider is mounted, so you can see exactly what applications do through
// the mount — and shows recursive vs direct watches, slow receivers, and
// cancellation.
//
//	go run ./examples/watch                    # scripted demo, no mount
//	go run ./examples/watch --mount ~/mnt/w    # then use the folder; Ctrl-C to stop
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"ella.to/simplefs"
	"ella.to/simplefs/memfs"
	"ella.to/simplefs/mount"
)

func main() {
	mountPath := flag.String("mount", "", "mount the watched provider here")
	flag.Parse()
	if err := run(*mountPath); err != nil {
		fmt.Fprintln(os.Stderr, "watch:", err)
		os.Exit(1)
	}
}

func run(mountPath string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	p := memfs.New()
	wp, ok := simplefs.As[simplefs.WatchProvider](p)
	if !ok {
		return fmt.Errorf("provider cannot be watched")
	}
	simplefs.MkdirAll(ctx, p, "/project/src", 0)

	// Recursive: every change below "/". Direct: "/project" and its
	// children only. Cancelling a watch's context closes its channel.
	all, err := wp.Watch(ctx, "/", true)
	if err != nil {
		return err
	}
	directCtx, stopDirect := context.WithCancel(ctx)
	defer stopDirect()
	direct, err := wp.Watch(directCtx, "/project", false)
	if err != nil {
		return err
	}
	go print("recursive", all)
	go print("direct   ", direct)

	if mountPath != "" {
		m, err := mount.Mount(ctx, p, mount.Options{Path: mountPath, Name: "watched"})
		if err != nil {
			return err
		}
		defer m.Close()
		fmt.Printf("mounted at %s — events appear as you use it; Ctrl-C to stop\n", m.Path())
		<-ctx.Done()
		return nil
	}

	simplefs.WriteFile(ctx, p, "/project/README.md", []byte("hi"), 0) // both see it
	simplefs.WriteFile(ctx, p, "/project/src/main.go", nil, 0)        // only recursive
	p.Rename(ctx, "/project/README.md", "/project/README.txt", simplefs.RenameOptions{})
	time.Sleep(100 * time.Millisecond)

	stopDirect() // the direct watch's channel closes; the recursive one continues
	time.Sleep(50 * time.Millisecond)
	simplefs.RemoveAll(ctx, p, "/project")

	// A receiver that falls behind misses events instead of blocking the
	// provider: this unread watch never slows down the writes below.
	slowCtx, stopSlow := context.WithCancel(ctx)
	defer stopSlow()
	if _, err := wp.Watch(slowCtx, "/", true); err != nil {
		return err
	}
	start := time.Now()
	for i := range 1000 {
		simplefs.WriteFile(ctx, p, fmt.Sprintf("/burst-%d", i%10), []byte("x"), 0)
	}
	took := time.Since(start)
	time.Sleep(100 * time.Millisecond) // let the printer catch up
	fmt.Printf("1000 writes with an unread watcher took %v (events beyond its buffer were dropped)\n",
		took.Round(time.Millisecond))
	return nil
}

func print(label string, events <-chan simplefs.Event) {
	n := 0
	for ev := range events {
		if n++; n > 12 {
			continue // keep the demo output short
		}
		if ev.OldPath != "" {
			fmt.Printf("%s %-6s %s (from %s)\n", label, ev.Type, ev.Path, ev.OldPath)
		} else {
			fmt.Printf("%s %-6s %s\n", label, ev.Type, ev.Path)
		}
	}
	fmt.Printf("%s channel closed\n", label)
}
