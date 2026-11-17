// Command nfsserver exposes the in-memory provider over NFSv3 (pure Go, no
// FUSE, no drivers). It has two modes:
//
// Serve (any platform, no privileges — the Docker story):
//
//	nfsserver                       # serve on :2049
//	nfsserver --addr :12049         # serve on another port
//	nfsserver --portmap             # also answer PORTMAP on :111 (Windows clients)
//
// The volume is then mounted from outside the process:
//
//	# Linux
//	mount -t nfs -o vers=3,proto=tcp,mountproto=tcp,port=2049,mountport=2049,nolock 127.0.0.1:/ /mnt/simplefs
//	# macOS
//	sudo mount_nfs -o vers=3,tcp,port=2049,mountport=2049,nolocks 127.0.0.1:/ /mnt/simplefs
//	# Windows (Client for NFS + --portmap)
//	mount -o anon,nolock \\127.0.0.1\simplefs Z:
//	# Docker: consume a containerized server as a named volume
//	docker volume create -d local -o type=nfs -o o=addr=127.0.0.1,port=2049,mountport=2049,vers=3,tcp,nolock -o device=:/ simplefs
//
// Mount (Linux with CAP_SYS_ADMIN, or Windows with Client for NFS): serve and
// attach it in one step through the mount package, until Ctrl-C:
//
//	nfsserver --mount /mnt/simplefs
//
// To ship your own backend, replace memfs.New() with any simplefs.FileProvider.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"ella.to/simplefs/memfs"
	"ella.to/simplefs/mount"
	"ella.to/simplefs/nfs"
)

func main() {
	addr := flag.String("addr", ":2049", "listen address (serve mode)")
	portmap := flag.Bool("portmap", false, "also answer PORTMAP on port 111 (needed by Windows clients)")
	uid := flag.Uint("uid", defaultID(os.Getuid()), "uid reported as the owner of every file")
	gid := flag.Uint("gid", defaultID(os.Getgid()), "gid reported as the owner of every file")
	mountPath := flag.String("mount", "", "serve and mount at this path (Linux: needs CAP_SYS_ADMIN)")
	flag.Parse()

	provider := memfs.New()

	if *mountPath != "" {
		mount.Register(provider)
		real, err := mount.MountAt("to.ella.simplefs.nfs", "SimpleFS", *mountPath)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("mounted at %s (via %s) — Ctrl-C to unmount\n", *mountPath, real)
		wait()
		if err := mount.UnmountAt("to.ella.simplefs.nfs", *mountPath); err != nil {
			log.Fatal(err)
		}
		return
	}

	srv := nfs.NewServer(provider, nfs.Options{
		UID:     uint32(*uid),
		GID:     uint32(*gid),
		Portmap: *portmap,
	})
	if err := srv.Start(*addr); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("serving NFSv3 on %s (port %d) — Ctrl-C to stop\n", *addr, srv.Port())
	fmt.Printf("mount it:  mount -t nfs -o vers=3,proto=tcp,mountproto=tcp,port=%d,mountport=%d,nolock <host>:/ /mnt/simplefs\n",
		srv.Port(), srv.Port())
	wait()
	srv.Close()
}

// defaultID clamps the -1 that os.Getuid/Getgid return on Windows to 0.
func defaultID(v int) uint {
	if v < 0 {
		return 0
	}
	return uint(v)
}

func wait() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
}
