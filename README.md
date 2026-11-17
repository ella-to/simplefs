# simplefs — a multi-platform FileProvider interface for Go

simplefs defines a platform-neutral filesystem provider interface (`simplefs.FileProvider`)
and brings implementations of it into the operating system. You implement one
path-based Go interface using plain Go types; simplefs handles the rest — with
**no kernel extensions and no FUSE** on any platform:

- **macOS** — backs a **File Provider** extension
  (`NSFileProviderReplicatedExtension`, the mechanism iCloud Drive and Dropbox use).
- **Linux** — a pure-Go userspace **NFSv3 server** (`nfs/`) attached through the
  kernel's built-in NFS client. Works inside Docker: an *unprivileged* container
  serves the filesystem, and the host or any other container mounts it —
  including as a native `docker volume`.
- **Windows** — the same NFS server, mounted with Windows' built-in
  **Client for NFS** (`mount.exe`, Pro/Enterprise/Education).

The whole stack is owned by this repository — there are no dependencies.

## Layout

```
.                      THE INTERFACE (package simplefs, at the root)
  simplefs.go          core types + FileProvider + FileHandle
  capabilities.go      optional capability interfaces (symlink, xattr, watch, ...)
  errors.go            sentinel errors (ErrNotExist, ...) + PathError
  path.go              logical-path helpers (CleanPath, JoinPath, ...)

memfs/                 in-memory simplefs.FileProvider (all capabilities) — for
                       testing and as the reference implementation

nfs/                   pure-Go NFSv3 server for any simplefs.FileProvider:
                       ONC RPC (TCP+UDP), PORTMAP v2, MOUNT v3, NFS v3, NLM v4.
                       This is the Linux/Windows/Docker mount mechanism.

mount/                 BRINGING A PROVIDER INTO THE SYSTEM
  provider.go          wire types (Item, Perm, Code, Error), Register + registry
  bridge.go            Bridge: path-based simplefs.FileProvider -> ID-based item model (pure Go)
  mount_darwin.go      Mount/MountAt/Unmount/List — wraps NSFileProviderManager (cgo)
  mount_nfs.go         Linux+Windows: in-process nfs.Server per mounted domain
  mount_linux.go       attach via mount(2) — no nfs-utils needed, Docker-friendly
  mount_windows.go     attach via mount.exe (built-in "Client for NFS")
  mount_other.go       stubs for everything else so dependent code still builds

capi/                  CGO bridge: //export symbols the Obj-C extension calls

macos/                 reusable Objective-C File Provider host
  ext/                 the .appex (forwards every system request to capi)
  app/                 Info.plist + entitlements for the container app bundle

examples/
  memfs/main.go        runnable demo: exercises everything in memory, no mount
  nfsserver/main.go    the NFS server binary: serve on any platform (no
                       privileges), or serve+mount in one step on Linux/Windows
  archive/main.go      c-archive entry: registers memfs, blank-imports capi
  mountapp/main.go     container app (Go): mounts/unmounts via mount.MountAt,
                       exposing the folder at a path you choose (--path)

Dockerfile             containerized NFS server (see "Linux / Docker" below)
scripts/docker-e2e.sh  end-to-end test: serve in one container, mount in another
```

## The interface you implement

Everything is path-based (`/docs/report.pdf`), context-aware, and uses plain
Go types, so a provider is equally at home over RPC, a database, S3, or RAM:

```go
type FileProvider interface {
    Stat(ctx context.Context, path string) (FileInfo, error)
    Open(ctx context.Context, path string, opts OpenOptions) (FileHandle, error)
    ReadDir(ctx context.Context, path string, page Page) (DirPage, error)
    Create(ctx context.Context, path string, opts CreateOptions) (FileHandle, error)
    Mkdir(ctx context.Context, path string, opts MkdirOptions) error
    Remove(ctx context.Context, path string, opts RemoveOptions) error
    Rename(ctx context.Context, oldPath, newPath string, opts RenameOptions) error
}
```

`FileHandle` is random-access (`io.ReaderAt`/`io.WriterAt` + `Stat`/`Sync`/
`Truncate`), because mounts issue concurrent reads and writes at arbitrary
offsets. `ReadDir` paginates for very large directories (a zero `Page` asks
for everything).

Optional capabilities are separate interfaces — implement the ones your
backend supports and mounts discover them with type assertions:
`SymlinkProvider`, `HardLinkProvider`, `ChmodProvider`, `ChownProvider`,
`ChtimesProvider`, `XattrProvider`, `StatFSProvider`, `WatchProvider`.

Return errors that wrap the sentinels in `errors.go` (`simplefs.ErrNotExist`,
`simplefs.ErrExist`, `simplefs.ErrPermission`, ...) — the mount layer maps them onto the
native error codes Finder shows the user. For an exact code (e.g. quota), a
provider may return a `*mount.Error` directly.

## Try it in memory (no mount, any platform)

```bash
go run ./examples/memfs     # or: make example
go test ./...
```

The example creates and updates files, lists with pagination, renames,
watches events, and uses the optional capabilities — everything stored in
process memory through the `memfs` package.

## Mount it for real (Linux / Docker)

Linux has no FileProvider-style userspace mount API, and FUSE needs a kernel
module plus `/dev/fuse`. simplefs sidesteps both: `nfs.Server` speaks NFSv3 in
pure Go (serving needs **no privileges at all**), and every Linux kernel ships
the NFS *client*. The two halves can live in the same process, in different
containers, or on different machines.

### Serve in Docker, consume the volume outside

```bash
make docker-image
docker run -d --name simplefs -p 2049:2049 simplefs-nfs   # unprivileged!
```

Mount it anywhere the container's port is reachable:

```bash
# Linux host
sudo mount -t nfs -o vers=3,proto=tcp,mountproto=tcp,port=2049,mountport=2049,nolock \
    127.0.0.1:/ /mnt/simplefs

# macOS host
sudo mount_nfs -o vers=3,tcp,port=2049,mountport=2049,nolocks 127.0.0.1:/ /mnt/simplefs
```

Or hand it to other containers as a **named volume** — Docker's built-in
`local` driver speaks NFS, so no plugin is needed:

```bash
IP=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' simplefs)
docker volume create -d local -o type=nfs \
    -o o=addr=$IP,port=2049,mountport=2049,vers=3,tcp,nolock -o device=:/ simplefs
docker run --rm -v simplefs:/data alpine sh -c 'echo hi > /data/hello && cat /data/hello'
```

`make docker-test` runs the full end-to-end suite (serve container + client
container that kernel-mounts it and exercises I/O, plus the in-container
mount below).

### Mount from Go on Linux

The same `mount.Mount`/`mount.MountAt` API as macOS works on Linux: it starts
an in-process `nfs.Server` on loopback and attaches it with the raw
`mount(2)` syscall. The kernel performs the NFS MOUNT handshake itself, so
even a `FROM scratch` image needs no `nfs-utils` — just `CAP_SYS_ADMIN`
(root, or `docker run --cap-add SYS_ADMIN`):

```go
mount.Register(memfs.New())
real, err := mount.MountAt("my.domain", "My Backend", "/mnt/simplefs")
...
mount.UnmountAt("my.domain", "/mnt/simplefs")
```

Or from the shell: `go run ./examples/nfsserver --mount /mnt/simplefs`.
To serve without mounting (unprivileged), use `nfs.Server` directly — that is
all `examples/nfsserver` does in its default mode.

### Owner mapping

NFSv3 carries numeric uid/gid. The provider model is single-user, so the
server reports one owner for every file (`nfs.Options.UID/GID`;
`examples/nfsserver` defaults to the serving user, flags `--uid/--gid`
override — set them to the *mounting* user's ids to avoid permission
surprises).

## Mount it for real (Windows)

Windows' no-driver options are the Cloud Files API (a full sync-engine model,
like macOS File Provider — a natural future backend) and its built-in NFS
client. simplefs uses the NFS client today, so the Linux and Windows paths
share one implementation:

1. Enable the feature once (Windows Pro/Enterprise/Education):
   Control Panel → Programs → Turn Windows features on or off →
   Services for NFS → **Client for NFS**.
2. `mount.Mount` starts the in-process server on `127.0.0.1:2049` (plus the
   portmapper on 111, which `mount.exe` requires) and maps a free drive
   letter:

```go
mount.Register(memfs.New())
drive, err := mount.Mount("my.domain", "My Backend")   // e.g. "Z:\"
```

Or manually against any simplefs NFS server (local or a Docker host):

```cmd
go run .\examples\nfsserver --portmap
mount -o anon,nolock \\127.0.0.1\simplefs Z:
```

Windows Home lacks the NFS client; a Cloud Files API backend would cover it
and is the intended follow-up.

## Mount it for real (macOS)

You need Go and the Xcode Command Line Tools (`clang`, `codesign`). No Xcode app.

```bash
make mount                      # mount, exposed at ~/simplefs by default
make mount MOUNT_PATH=~/mydir   # ... or at a path you choose
```

macOS always places the real mount under `~/Library/CloudStorage/<Name>-<DomainID>`
— there is no API to mount elsewhere — so `MOUNT_PATH` is a symlink to that
location; the folder looks and behaves as if it were mounted at your path.

### REQUIRED one-time enable (macOS 15.2+)

A newly added third-party File Provider domain starts **disabled** for
privacy; there is no API to self-enable it. Do it once (`make settings` opens
the pane):

```
System Settings → General → Login Items & Extensions
  → Extensions → (i) next to "File Providers" → toggle "SimpleFS" ON
```

Until enabled, the folder is empty and `ls`/`cat` hang, and the daemon logs
`FP -2011 "Sync is not enabled"`.

Then:

```bash
echo hello > ~/simplefs/hello.txt && cat ~/simplefs/hello.txt
make events     # tail the changes as the Go provider observes them (WatchProvider)
make unmount    # removes the domain and the ~/simplefs symlink
```

Everything you do in that folder lands in `memfs` — an in-memory filesystem —
via `simplefs.FileProvider`. It resets whenever macOS relaunches the extension;
back it with real storage to persist.

### How the pieces connect

1. `examples/archive` calls `mount.Register(yourProvider)` and blank-imports
   `capi`; built with `-buildmode=c-archive` it becomes `libprovider.a`.
2. The Objective-C extension (`macos/ext`) is compiled against
   `capi/bridge.h` and linked with that archive; every system request
   (enumerate, fetch, create, modify, delete) becomes a `Provider*` call into Go.
3. `mount.Bridge` translates those ID-based calls into path-based
   `simplefs.FileProvider` calls, keeping a stable ID↔path table across renames.
4. The container app (`examples/mountapp`, a Go binary built into the bundle
   with the `macos/app` plist/entitlements) adds the File Provider domain via
   `mount.MountAt`; macOS mounts it under `~/Library/CloudStorage` and the
   `--path` symlink exposes it where you asked.

To ship your own backend, replace `memfs.New()` in `examples/archive/main.go`
with your `simplefs.FileProvider` — the host, bridge, and build stay the same.

### Mounting from Go

The container app is itself Go (`examples/mountapp`); any Go app in the bundle
can manage mounts the same way:

```go
real, err := mount.MountAt("to.ella.simplefs.domain", "My Backend", home+"/simplefs") // symlinks ~/simplefs -> real
real, err = mount.Mount("to.ella.simplefs.domain", "My Backend")                  // ~/Library/CloudStorage/... only
domains, err := mount.List()
err = mount.UnmountAt("to.ella.simplefs.domain", home+"/simplefs") // removes domain + symlink
```

It must run from inside the signed bundle that embeds the extension (macOS
pairs them through the shared App Group); on other platforms these return an
error.

## Signing notes

The build uses **ad-hoc** signing (`IDENTITY=-`), enough for local use. For a
real certificate: `make IDENTITY="Apple Development: you@example.com (TEAMID)" mount`.

Requirements the repo already satisfies:

- **App Group** shared by app and extension (`group.to.ella.simplefs` in both
  entitlements, plus `NSExtensionFileProviderDocumentGroup` in
  `macos/ext/Info.plist`). Missing this causes `addDomain` error `-2001`.
- **Registration**: `make install` runs `lsregister`, launches the app once,
  then `pluginkit -a` / `pluginkit -e use`. Skipping any of these yields
  `-2014` (`ApplicationExtensionNotFound`). Re-run `make mount` after a rebuild.

| `addDomain` error | Meaning | Fix |
| --- | --- | --- |
| `-2001` "application cannot be used right now" | umbrella; check underlying error | see below |
| `-2014` `ApplicationExtensionNotFound` | extension not *launchable* | `make install`, app in `~/Applications` |
| `-2011` `DomainDisabled` "Sync is not enabled" | domain added but disabled | enable in System Settings (above) |

Diagnostics:

```bash
pluginkit -m -i to.ella.simplefs.ext                       # "+" prefix = enabled
log show --last 1m --predicate 'process == "fileproviderd"' --info --debug
```
