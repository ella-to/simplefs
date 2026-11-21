# simplefs guide

simplefs lets you write a filesystem once — as a small Go interface — and use
it from Go, serve it over NFS or WebDAV, or mount it as a real folder on
Linux, macOS, and Windows, with no FUSE, kernel extension, or driver.

Every snippet here is taken from a runnable program in [`examples/`](examples);
`sh scripts/build-examples.sh` builds them all for every OS.

Contents:

1. [Concepts](#1-concepts)
2. [Quick start](#2-quick-start)
3. [Implementing a provider](#3-implementing-a-provider)
4. [Capabilities and wrappers](#4-capabilities-and-wrappers)
5. [Using a provider from Go](#5-using-a-provider-from-go)
6. [Serving over NFS](#6-serving-over-nfs)
7. [Mounting](#7-mounting)
8. [Docker](#8-docker)
9. [macOS File Provider](#9-macos-file-provider)
10. [Windows](#10-windows)
11. [Performance tuning](#11-performance-tuning)
12. [Edge cases and FAQ](#12-edge-cases-and-faq)
13. [Troubleshooting](#13-troubleshooting)

---

## 1. Concepts

A **provider** stores files: it implements `simplefs.FileProvider` (six
methods) and, optionally, capability interfaces such as `SymlinkProvider`.
The repository ships three: `memfs` (in memory), `dirfs` (a local directory),
and `examples/sqlitefs` (SQLite); `simplefs.FromFS` turns any `io/fs.FS` into
a read-only one.

A **backend** exposes a provider to something that is not Go:

| package | what it does |
|---|---|
| `nfs` | pure-Go NFSv3 server (TCP+UDP, MOUNT, PORTMAP, NLM stub) |
| `webdav` | pure-Go WebDAV server (`http.Handler`) |
| `mount` | runs one of those on loopback and attaches it with the OS's own client |
| `fileprovider` | macOS File Provider extension (Finder sidebar, on-demand download) |

```
Linux/macOS/Windows: OS NFS or WebDAV client ─loopback─> nfs.Server / webdav.Handler ─> FileProvider
macOS File Provider: fileproviderd ─> .appex (ObjC) ─cgo─> capi ─> fileprovider.Bridge ─> FileProvider
```

**Logical paths.** Providers see `/`-separated, absolute, clean paths
(`/docs/a.txt`), whatever the OS. Every method must treat its input as
`simplefs.CleanPath(p)` would, which also means `..` can never climb above
`/`. Names are byte strings: no case folding, no Unicode normalization.

**Platform matrix**

| OS | `mount` backend | privileges | status |
|---|---|---|---|
| Linux | NFS via `mount(2)` | `CAP_SYS_ADMIN` (root, or `--cap-add SYS_ADMIN`) | tested (CI, Docker) |
| Linux, unprivileged | — serve with `nfs.Server`, mount from outside | none | tested |
| macOS | NFS via `/sbin/mount_nfs`; WebDAV via `mount_webdav` | none | tested |
| macOS (Finder-integrated) | `fileprovider` | signed app bundle + one-time enable | builds; manual test |
| Windows Pro/Enterprise/Server | NFS via `mount.exe` (Client for NFS) | none | builds; untested in CI |
| Windows Home | WebDAV via `net use` (WebClient) | none | builds; untested in CI |
| FreeBSD | NFS via `mount_nfs` | root | compiles |
| anything else | `mount.ErrUnsupported`; `nfs` and `webdav` still work | — | compiles |

How each backend uses capabilities:

| capability | NFS | WebDAV | File Provider |
|---|---|---|---|
| Symlink | yes | no | yes (shown as links) |
| HardLink | yes (`FileInfo.FileID`/`Links` for inode and nlink) | no | no |
| Chmod / Chown / Chtimes | yes / yes / yes | Chtimes (PROPPATCH) | no |
| StatFS | `df`, read-only detection | quota properties | no |
| Watch | invalidates the attribute cache | no | change tracking (`signalEnumerator`) |
| Xattr | no (NFSv3 has none) | no | no |

## 2. Quick start

```go
p := memfs.New()
simplefs.MkdirAll(ctx, p, "/docs", 0)
simplefs.WriteFile(ctx, p, "/docs/hello.txt", []byte("hi"), 0o644)
data, _ := simplefs.ReadFile(ctx, p, "/docs/hello.txt")

// …and as a real folder:
m, err := mount.Mount(ctx, p, mount.Options{Path: "/tmp/demo"}) // Windows: "Z:"
if err != nil { /* see section 13 */ }
defer m.Close()
```

Run `go run ./examples/basic` for a tour of the API and
`go run ./examples/mount` to mount (Linux: `sudo`).

## 3. Implementing a provider

Start from [`examples/customprovider`](examples/customprovider) (read-only,
about 200 lines) or [`examples/sqlitefs`](examples/sqlitefs) (read-write,
chunked storage, transactions). Then prove it with the conformance suite:

```go
func TestConformance(t *testing.T) {
	providertest.Run(t, func(t *testing.T) simplefs.FileProvider {
		return myfs.New(t.TempDir())
	}, providertest.Options{})
}
```

`providertest` checks the contract written on `simplefs.FileProvider` and
`simplefs.FileHandle`. Use `Options.ReadOnly` for read-only providers, and
`CaseInsensitive`/`NormalizesUnicode` when the storage folds names
(NTFS, APFS). Capability tests are skipped when the provider lacks the
interface or returns `ErrNotSupported`.

### The contract as a checklist

- **Paths**: clean every input; return clean `FileInfo.Path`; `/` always
  exists as a directory; removing or renaming `/` is `ErrInvalid`.
- **Stat** does not follow a final symlink.
- **Open**: neither Read nor Write means Read; `Truncate`/`Append` without
  `Write` is `ErrInvalid`; a directory is `ErrIsDir`; `Create` with a missing
  parent is `ErrNotExist`; `Create|Exclusive` on an existing file is
  `ErrExist`; `Mode` (default 0o644) applies only when a file is created.
- **Handles**: `ReadAt` at or past EOF → `0, io.EOF`; a short read →
  `n, io.EOF`; `WriteAt` past EOF zero-fills; with `Append` the offset is
  ignored; writing a read-only handle → `ErrPermission`; any use after
  `Close` (including a second `Close`) → `ErrClosed`. Methods are called
  concurrently — `ReadAt`/`WriteAt` at different offsets at the same time
  must be safe.
- **Handle lifetime**: a handle must survive a rename of its file (and
  `Stat` must report the new path). After a remove it may keep working, as on
  POSIX, or fail with `ErrNotExist`.
- **ReadDir** never returns `.`/`..`; listing a file is `ErrNotDir`; a zero
  `Page` returns everything; pages of an unchanging directory contain every
  entry exactly once; a directory changing between pages must not make the
  provider loop or panic. Fill `DirEntry.Info` whenever it is cheap.
- **Mkdir**: existing → `ErrExist`; missing parent → `ErrNotExist`.
- **Remove**: missing → `ErrNotExist` (nil with `Force`); non-empty dir →
  `ErrNotEmpty` unless `Recursive` — or return `ErrNotSupported` for
  `Recursive` and let `simplefs.RemoveAll` walk the tree.
- **Rename**: onto itself is a no-op; existing destination → `ErrExist`
  without `Overwrite`; file over dir `ErrIsDir`, dir over file `ErrNotDir`,
  dir over non-empty dir `ErrNotEmpty`; into its own subtree `ErrInvalid`;
  the whole subtree moves; overwriting should be atomic for readers.
- **Context**: honour cancellation — return an error wrapping `ctx.Err()`.
- **Concurrency**: every method may be called from many goroutines at once.

### Which error to return

Wrap a sentinel, ideally in a `*simplefs.PathError` (it *is*
`io/fs.PathError`, and the common sentinels *are* the `io/fs` ones, so
`os.IsNotExist`-style checks keep working):

| return | when | NFS | WebDAV | File Provider |
|---|---|---|---|---|
| `ErrNotExist` | no such path | NOENT | 404 | not found |
| `ErrExist` | path exists | EXIST | 412 | name conflict |
| `ErrNotEmpty` | directory has entries | NOTEMPTY | 409 | name conflict |
| `ErrIsDir` / `ErrNotDir` | wrong type | ISDIR / NOTDIR | 409 | unsupported |
| `ErrPermission` | not allowed | ACCES | 403 | not permitted |
| `ErrReadOnly` | read-only filesystem | ROFS | 403 | not permitted |
| `ErrNoSpace` | quota, disk full | NOSPC | 507 | quota |
| `ErrTooLarge` | file size limit | FBIG | 413 | quota |
| `ErrNameTooLong` | name limit | NAMETOOLONG | 414 | unsupported |
| `ErrNotSupported` | operation not implemented | NOTSUPP | 501 | unsupported |
| `ErrInvalid` | bad argument | INVAL | 400 | I/O error |
| `ErrBusy`, `context.DeadlineExceeded` | transient, retry later | JUKEBOX (client retries) | 503 | server (retried) |
| anything else | — | IO (logged) | 500 | I/O error |

### ETag, FileID, Links

- `FileInfo.ETag` is optional; when set it must change on every content or
  metadata change. Backends use it for cache validation (WebDAV `ETag`,
  File Provider item versions). Leave it empty rather than risk a stale one
  (e.g. `mtime-size` with coarse timestamps).
- `FileInfo.FileID` identifies the underlying object (an inode number) and
  `Links` counts hard links. Set them for every object or none; NFS reports
  them as `fileid`/`nlink`.

### Pagination tokens

Tokens are opaque to callers. The simplest scheme is to list in a stable
order and use the last returned name as the token (`memfs`, `dirfs`,
`FromFS`, `sqlitefs` all do): continuation is a binary search, and an entry
deleted between pages cannot shift later pages.

## 4. Capabilities and wrappers

Optional interfaces: `SymlinkProvider`, `HardLinkProvider`, `ChmodProvider`,
`ChownProvider` (-1 = unchanged), `ChtimesProvider` (zero time = unchanged),
`XattrProvider`, `StatFSProvider`, `WatchProvider` (must never block the
provider; slow receivers drop events). Implement what your storage can do;
return `ErrNotSupported` from a capability that cannot serve a call.

Always discover capabilities with `simplefs.As`, not a type assertion — it
also looks through middleware:

```go
if sp, ok := simplefs.As[simplefs.SymlinkProvider](p); ok { … }
```

Wrappers ([`examples/wrappers`](examples/wrappers)):

- `simplefs.ReadOnly(p)` — every mutation fails with `ErrReadOnly`;
  `StatFS` reports read-only.
- `simplefs.Sub(p, "/tenants/acme")` — a jailed view; paths in results and
  errors are relative to the new root.
- Your own middleware (logging, metrics, caching): embed the inner provider
  and implement `Unwrap() FileProvider` **only if** calls reaching the inner
  provider directly behave the same. Wrappers that change paths or refuse
  operations implement every capability themselves (as `Sub` and `ReadOnly`
  do), or `As` would bypass them.

## 5. Using a provider from Go

Helpers that work on any provider: `ReadFile`, `WriteFile`, `Create`,
`Exists`, `MkdirAll`, `RemoveAll`, `Entries` (an iterator that drains
pagination), `WalkDir` (`fs.SkipDir`/`fs.SkipAll` work), and `Copy`
(streaming, 1 MiB chunks, checks ctx between chunks).

A `FileHandle` is an `io.ReaderAt` + `io.WriterAt`, so
`io.NewSectionReader(h, 0, size)` and `io.NewOffsetWriter(h, 0)` adapt it to
any `io.Reader`/`io.Writer` API ([`examples/largefiles`](examples/largefiles)).

**io/fs** ([`examples/iofs`](examples/iofs)):

- `simplefs.FS(ctx, p)` is an `fs.FS` (also `ReadDirFS`, `ReadFileFS`,
  `StatFS`, `SubFS`; files are `io.ReaderAt`+`io.Seeker`). Use it with
  `fs.WalkDir`, `fs.Glob`, `template.ParseFS`, `http.FileServerFS`. It follows
  symlinks (absolute targets resolve against the provider root), as io/fs
  expects; `ReadDir` entries describe the links themselves.
- `simplefs.FromFS(fsys)` serves an `embed.FS`, `zip.Reader`, `os.DirFS`, … as
  a read-only provider.

## 6. Serving over NFS

```go
srv := nfs.NewServer(p, nfs.Options{
	AllowedClients: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, // default: loopback only
	ReadOnly:       false,
	AttrCacheTTL:   time.Second, // see section 11
	Logger:         slog.Default(),
})
if err := srv.Start(":2049"); err != nil { … }
defer srv.Shutdown(ctx) // drains in-flight RPCs, syncs cached write handles
```

`examples/nfsserver` is a ready-made binary (`--backend mem|dir`,
`--allow`, `--readonly`, `--portmap`, `--uid/--gid`, `--mount`).

**Security.** NFSv3 here has no authentication (AUTH_SYS is accepted, not
enforced). The server therefore accepts only loopback clients unless
`AllowedClients` says otherwise; never expose it beyond networks you trust.
`ReadOnly` refuses every mutating procedure before it reaches the provider.

**Owners.** NFS carries numeric uid/gid; the provider model has no owner, so
every file is reported as `Options.UID/GID`. Set them to the *mounting*
user's ids (the default is the serving process's) or files look foreign.
SETATTR uid/gid is forwarded to `ChownProvider`.

**Several providers on one server.** `srv.Export("photos", p2)`; clients
mount `server:/photos`. Handles name their export; RENAME/LINK across exports
is `XDEV`; `Unexport` makes its handles stale.

**Client commands** (the server prints them on start):

```sh
# Linux (no rpcbind needed: ports are explicit)
sudo mount -t nfs -o vers=3,proto=tcp,mountproto=tcp,port=2049,mountport=2049,nolock,soft HOST:/ /mnt/x
# macOS
mount_nfs -o vers=3,tcp,port=2049,mountport=2049,noresvport,locallocks,soft HOST:/ ~/mnt/x
# Windows (server started with --portmap, on port 2049)
mount -o anon,nolock,mtype=soft \\HOST\simplefs Z:
```

**Locking.** NLM is a stub that grants every lock (`nolock` / `locallocks`
on clients keeps locking local). Two clients writing one file can interleave
— the same as two processes without `flock`.

**Retries.** Replies to non-idempotent calls are cached (1024 entries), so a
retransmitted REMOVE/RENAME/CREATE gets the original answer instead of
`NOENT`/`EXIST`. `Options.Timeout` bounds each RPC's provider calls; expiry
(or `ErrBusy`) reaches clients as JUKEBOX, which they retry.

## 7. Mounting

```go
m, err := mount.Mount(ctx, p, mount.Options{
	Path:     "/mnt/data", // "" = auto; Windows: "Z:"
	ReadOnly: false,
	Backend:  mount.Auto,  // NFS; WebDAV on Windows without Client for NFS
})
defer m.Close()             // or m.Unmount(ctx); m.Done() closes when unmounted
```

| OS | prerequisites | auto path |
|---|---|---|
| Linux | kernel NFS client (standard); `CAP_SYS_ADMIN` | `$XDG_RUNTIME_DIR/simplefs/NAME-*` (root: `/run/simplefs`) |
| macOS | nothing (user-owned mount point) | `~/Library/Caches/simplefs/NAME-*` |
| Windows | Client for NFS, or WebClient (WebDAV) | highest free drive letter |
| FreeBSD | root | `$TMPDIR/simplefs/NAME-*` |

The provider lives in **your** process. Mounts are *soft* with short
timeouts: if the process dies, operations on the mount fail with an I/O
error after roughly 10–15 s instead of hanging (macOS additionally
force-unmounts it after 30 s). `mount.CleanupStale(ctx)` detaches loopback
mounts whose server is gone; `Mount` calls it first. Always unmount on
shutdown — `examples/mount` shows signal handling.

Errors: `mount.ErrNoPrivilege` (Linux without CAP_SYS_ADMIN — or serve with
`nfs.Server` and mount from outside), `mount.ErrNoClient` (Client for NFS or
WebClient missing; the message names the fix), `mount.ErrUnsupported`.

The server side caches attributes for 1 s (`AttrCacheTTL`), matching the
client's `actimeo=1`: changes made directly to the provider (not through the
mount) appear within about a second, immediately with a `WatchProvider`.

## 8. Docker

Serving needs no privileges; only the kernel mount does.

```sh
make docker-image                                  # or: docker buildx build --platform linux/amd64,linux/arm64 -t simplefs-nfs .
docker run -d --name simplefs -p 2049:2049 simplefs-nfs   # unprivileged
```

- **Host mount**: Linux `sudo mount -t nfs -o vers=3,proto=tcp,mountproto=tcp,port=2049,mountport=2049,nolock 127.0.0.1:/ /mnt/x`;
  macOS `mount_nfs -o vers=3,tcp,port=2049,mountport=2049,noresvport,locallocks 127.0.0.1:/ ~/mnt/x`.
- **Named volume for other containers** ([`examples/docker/compose.yaml`](examples/docker/compose.yaml)):
  `docker volume create -d local -o type=nfs -o o=addr=SERVER_IP,port=2049,mountport=2049,vers=3,tcp,nolock -o device=:/ simplefs`.
  The daemon (on Docker Desktop/OrbStack/Colima: the Linux VM) performs the
  mount, so `addr` must be reachable from it — the server container's IP,
  never `127.0.0.1`.
- **In-container mount**: `docker run --cap-add SYS_ADMIN simplefs-nfs --mount /mnt/x` (add `--security-opt apparmor=unconfined` on hosts enforcing AppArmor, such as Ubuntu)
  (some hosts also need `--security-opt apparmor=unconfined`); share it with
  a bind mount using `:rshared` propagation.
- **Serve a host directory**: `-v "$PWD/data:/data" … --backend dir --dir /data`.
- **Image defaults**: `--allow 0.0.0.0/0,::/0` (every client in a container
  is remote); narrow it for anything exposed beyond your machine.
- **Tests**: `make docker-test` runs three scenarios (unprivileged server +
  kernel-mounting client; in-container `mount.Mount`; a killed server that must
  fail soft and be cleaned up). It works on Linux, and on macOS/Windows with
  Docker Desktop, OrbStack, or Colima (from WSL2 or Git Bash on Windows).
  For arm64 on amd64 hosts: `docker run --privileged --rm tonistiigi/binfmt --install arm64`.

## 9. macOS File Provider

`mount` already works on macOS with no bundle. Choose `fileprovider` when you
want Finder integration: a sidebar entry under Locations, files downloaded on
demand and cached/evicted by the system, sync badges. The cost: a signed app
bundle, a one-time manual enable, and macOS-only cgo.

```sh
make mount                      # build, sign (ad hoc), install, add the domain, link ~/simplefs
make settings                   # then: Login Items & Extensions > File Providers > SimpleFS ON
make events                     # tail provider events
make unmount
```

How it fits together: `examples/fileprovider/archive` calls
`fileprovider.Register(p)` and blank-imports `fileprovider/capi`; it is built
as a c-archive and linked into the Objective-C extension
(`fileprovider/host/ext`). The container app (`examples/fileprovider/app`)
adds the domain with `fileprovider.AddDomainAt`. To ship your own backend,
replace `memfs.New()` in the archive.

What the bridge does for you: stable item IDs persisted across extension
relaunches (a journal under `HostDir()`), content streamed through files (any
size), paged enumeration, change tracking from `WatchProvider` (providers
without it are re-enumerated every 30 s), symlinks as links.

| `addDomain` error | meaning | fix |
|---|---|---|
| `-2001` | umbrella "cannot be used right now" | check the underlying error |
| `-2014` `ApplicationExtensionNotFound` | extension not launchable | `make install`; app in `~/Applications` |
| `-2011` "Sync is not enabled" | domain disabled (macOS 15.2+) | enable it in System Settings |

Diagnostics: `pluginkit -m -i to.ella.simplefs.ext` (`+` = enabled);
`log show --last 1m --predicate 'process == "fileproviderd"' --info --debug`.

## 10. Windows

- **NFS (Pro, Enterprise, Education, Server)**: enable Client for NFS once
  (admin PowerShell):
  `Enable-WindowsOptionalFeature -Online -All -FeatureName ServicesForNFS-ClientOnly,ClientForNFS-Infrastructure`
  (Server: `Install-WindowsFeature NFS-Client`). `mount.exe` can only talk to
  port 2049 and needs a portmapper on 111, so all `mount.Mount` calls in a
  process share one server on those ports (each provider is an export, each
  mount a drive letter). Nothing else on the machine may use 2049/111.
- **WebDAV (every edition, including Home)**: `Backend: mount.WebDAV`, or
  `Auto` when `mount.exe` is missing. Uses the WebClient service (`net use`).
  Limits to know: WebClient caps files at **50 MB** by default (raise
  `HKLM\SYSTEM\CurrentControlSet\Services\WebClient\Parameters\FileSizeLimitInBytes`,
  then restart WebClient); it may need starting (`sc start WebClient`); it is
  slower than NFS for many small files.
- uid/gid are meaningless on Windows (mounted anonymously).
- These code paths compile and follow the documented tools, but are not
  exercised in CI; please report problems with the full error text.

## 11. Performance tuning

For providers:

- **Fill `DirEntry.Info`.** Otherwise every listed entry costs a `Stat`
  (READDIRPLUS, Finder, `ls -l`).
- **Make `Stat` cheap**; it is the most frequent call.
- **Support random access in handles**: the NFS server keeps one handle open
  per file across READ/WRITE calls (closed after `HandleIdle`, default 5 s),
  and clients send 1 MiB chunks out of order.
- **Implement `WatchProvider`** if anything changes the data behind the
  server's back: caches are then invalidated immediately.
- Return `ErrBusy` for transient failures: NFS clients retry instead of
  failing the application.

For the server (`nfs.Options`):

- `AttrCacheTTL` (off by default; `mount` uses 1 s): reuse `Stat` results.
  It cut a LOOKUP/GETATTR/ACCESS storm from 4 provider calls to ~0 on a
  1 ms-per-call backend in our benchmarks. Larger values mean staler
  attributes for changes made outside the server (without Watch).
- Directory listings are snapshotted per READDIR sequence: listing 50k
  entries costs ~100 provider calls instead of ~20,000.
- `MaxHandles` (default 1<<20) bounds remembered filehandles; `MaxInFlight`
  (64) bounds concurrent RPCs per connection; `Timeout` bounds each RPC.

Benchmarks: `go test -run '^$' -bench . -benchmem ./nfs` (the `slow`
variants simulate a remote backend).

## 12. Edge cases and FAQ

**Case sensitivity.** Providers are case-sensitive byte stores. macOS and
Windows clients assume case-insensitive volumes: creating `Readme` next to
`README` works through the mount, but some apps get confused. If your
users are on those systems, make the provider case-insensitive (store a
folded key) and run `providertest` with `CaseInsensitive: true`.

**Unicode NFC/NFD.** macOS often sends decomposed names (NFD). simplefs never
normalizes, so `café` (NFC) and `café` (NFD) are different files. Normalize
in the provider if your backend must match names from other systems.

**Names Windows cannot use** (`: \ * ? " < > |`, `CON`, `NUL`, `COM1`, a
trailing dot or space): fine in a provider, but a Windows client cannot open
them. `dirfs` rejects them on Windows with `ErrInvalid`. Avoid creating them
if Windows users are expected.

**Names over 255 bytes** fail with `ErrNameTooLong` (`simplefs.ValidName`).

**Files over 4 GiB** work everywhere (64-bit offsets; NFS max file size
2^62). WebDAV on Windows is limited by WebClient (see section 10).

**Sparse files.** `WriteAt` past EOF and growing `Truncate` must read back as
zeros; `memfs` allocates them, `dirfs` and `sqlitefs` keep holes.

**Empty files, zero mtime, mode 0.** All valid. NFS reports mode 0 as 0644
(files), 0755 (dirs), or 0777 (links), and a zero time as the epoch.

**`.DS_Store`, `._*`, `Thumbs.db`, `desktop.ini`.** macOS and Windows clients
create these. Filter them in your provider (return `ErrPermission` on create,
or store them somewhere disposable) if they bother your backend; macOS
`._*` AppleDouble files appear on NFS volumes because NFSv3 has no xattrs.

**Deleting or renaming open files.** A handle must survive a rename. After a
remove it may keep working (POSIX) or fail with `ErrNotExist`; NFS clients
"silly-rename" deleted-but-open files themselves on some systems.

**Concurrent writers.** No locking is enforced (NLM grants everything). Two
writers to one file interleave at the chunk level. Use a lock in your app or
a provider-level scheme if you need exclusion.

**`O_APPEND` over NFS.** NFSv3 has no append: the client computes the offset
from its cached size, so concurrent appenders on different machines can
overwrite each other. Within one client it works.

**Read-only providers.** Return `ErrReadOnly` for every mutation and report
`FSInfo.ReadOnly`, or wrap with `simplefs.ReadOnly`; mount with
`ReadOnly: true` so clients know up front.

**Quota and disk full.** Return `ErrNoSpace` (or `ErrTooLarge` for per-file
limits): applications see ENOSPC/EFBIG and Finder shows a quota alert.

**Slow or failing backends.** Return errors wrapping `ctx.Err()` promptly;
use `ErrBusy` for "try again". Set `nfs.Options.Timeout` to bound how long a
client waits. Soft mounts turn a dead server into I/O errors, not hangs.

**Context cancellation.** The NFS server cancels a request's context when its
client disconnects or the server closes; `fileprovider` and the helpers pass
the caller's context through.

**Crash recovery and stale mounts.** See section 7: soft mounts plus
`mount.CleanupStale`. Data durability is the provider's job — `Sync` must make
writes durable (NFS COMMIT and stable WRITEs call it).

**Multiple mounts in one process.** Fine: one server per mount on Unix, one
shared server with an export per mount on Windows.

**Hard links.** Provide `FileInfo.FileID` and `Links`, or each name looks like
a separate file (same content, different inode).

**Symlinks resolve on the client.** A target like `/etc/passwd` resolves on
the *client* machine, not inside the provider; relative targets are safer.
`simplefs.FS` resolves absolute targets against the provider root instead.

**Timestamp precision.** NFS carries nanoseconds; WebDAV seconds; provider
precision varies (`dirfs`: the OS). Do not compare timestamps for equality
across backends.

**"Stale file handle" after a server restart.** Expected: handles carry a
per-server boot ID, so old ones are rejected instead of silently naming a
different file. Re-open the path (or remount).

## 13. Troubleshooting

| symptom | cause | fix |
|---|---|---|
| `ls` on the mount hangs | server process stopped (hard mount from another tool) | `umount -f` / `umount -l`; mounts made by `mount.Mount` are soft and fail within ~15 s |
| `Stale file handle` | server restarted, or handle evicted (`MaxHandles`) | re-open the path; remount |
| `access denied by server` / connection reset | client not in `AllowedClients` (default loopback) | `--allow CIDR` / `nfs.Options.AllowedClients` |
| `mount: … CAP_SYS_ADMIN` (`ErrNoPrivilege`) | Linux mount without privileges | `sudo`, `--cap-add SYS_ADMIN`, or serve and mount from outside |
| `Options.Portmap: cannot bind port 111` | rpcbind already running, or no privilege | stop rpcbind, or omit `--portmap` (only Windows clients need it) |
| `mount.exe not found` (`ErrNoClient`) | Client for NFS not enabled | see section 10, or `Backend: mount.WebDAV` |
| `net use` error 67 | WebClient service stopped | `sc start WebClient` |
| files over 50 MB fail on Windows WebDAV | WebClient size limit | raise `FileSizeLimitInBytes` |
| files owned by `nobody`/another user | uid/gid mapping | `--uid/--gid` (or `nfs.Options.UID/GID`) of the mounting user |
| changes made directly to the backend show up late | attribute cache | implement `WatchProvider`, or lower `AttrCacheTTL` |
| File Provider `-2001` / `-2011` / `-2014` | see section 9 | see section 9 |
| folder empty and `ls` hangs (File Provider) | domain disabled | `make settings`, enable SimpleFS |
