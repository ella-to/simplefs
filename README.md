# simplefs

Write a filesystem once as a small Go interface — then use it from Go, serve
it over NFS or WebDAV, or mount it as a real folder on Linux, macOS, and
Windows. No FUSE, no kernel extensions, no drivers: simplefs talks to the
network-filesystem clients every OS already ships. The root module has no
dependencies.

```go
type FileProvider interface {
	Stat(ctx context.Context, path string) (FileInfo, error)
	Open(ctx context.Context, path string, opts OpenOptions) (FileHandle, error)
	ReadDir(ctx context.Context, path string, page Page) (DirPage, error)
	Mkdir(ctx context.Context, path string, opts MkdirOptions) error
	Remove(ctx context.Context, path string, opts RemoveOptions) error
	Rename(ctx context.Context, oldPath, newPath string, opts RenameOptions) error
}
```

## Quick start

```go
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"ella.to/simplefs"
	"ella.to/simplefs/memfs"
	"ella.to/simplefs/mount"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	p := memfs.New() // or dirfs.New(dir), your own provider, simplefs.FromFS(embedFS)…
	simplefs.WriteFile(ctx, p, "/hello.txt", []byte("hi from Go\n"), 0o644)

	m, err := mount.Mount(ctx, p, mount.Options{Path: "/tmp/simplefs"}) // Windows: "Z:"
	if err != nil {
		panic(err)
	}
	defer m.Close()
	fmt.Println("mounted at", m.Path(), "— Ctrl-C to unmount")
	<-ctx.Done()
}
```

## Platforms

| OS | mount with | privileges |
|---|---|---|
| Linux | kernel NFS client (`mount(2)`) | `CAP_SYS_ADMIN`; or serve unprivileged and mount from outside |
| macOS | `mount_nfs` or `mount_webdav`; optional Finder-integrated File Provider | none |
| Windows | Client for NFS (`mount.exe`), or WebDAV (`net use`) on Home | none |
| Docker | unprivileged server container, consumed as a named NFS volume | none to serve |

## What is in the box

| package | |
|---|---|
| `simplefs` | the interface, capabilities, errors (`io/fs`-compatible), helpers, `FS`/`FromFS` io/fs bridges, `ReadOnly`/`Sub` wrappers |
| `memfs`, `dirfs` | in-memory and local-directory providers |
| `providertest` | conformance suite for your provider |
| `nfs` | pure-Go NFSv3 server, multiple exports |
| `webdav` | pure-Go WebDAV handler |
| `mount` | one call to mount on any OS |
| `fileprovider` | macOS File Provider extension backend |

## Learn more

- [guide.md](guide.md): writing providers, the contract, error mapping,
  serving, mounting, Docker, Windows, tuning, edge cases, troubleshooting.
- [examples/](examples): `basic`, `customprovider`, `iofs`, `dirfs`,
  `sqlitefs` (SQLite-backed, its own module), `nfsserver`, `mount`, `watch`,
  `wrappers`, `largefiles`, `docker`, `fileprovider`.

```sh
make check        # gofmt, vet, race tests, cross-OS vet
make docker-test  # end-to-end mounts in Docker
go test -tags mounttest ./mount/   # real mounts (Linux: as root)
```
