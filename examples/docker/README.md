# simplefs in Docker

`compose.yaml` runs the NFS server (`examples/nfsserver`, memfs backend) in an
unprivileged container and mounts it into a second container as a named
volume through Docker's built-in `local` driver.

```sh
docker compose -f examples/docker/compose.yaml up --build --abort-on-container-exit
docker compose -f examples/docker/compose.yaml down -v
```

Variations:

- **Serve a host directory** instead of memory: add
  `volumes: ["./data:/data"]` to `nfs` and append `--backend dir --dir /data`
  to its command.
- **Mount on the host**: publish `2049:2049` and use the commands that
  `nfsserver` prints at startup (Linux `mount -t nfs`, macOS `mount_nfs`).
- **Mount inside one container** (needs `cap_add: [SYS_ADMIN]`): run
  `nfsserver --mount /mnt/simplefs`; see `scripts/docker-e2e.sh` scenario B.

See `guide.md` ("Docker") for the full matrix of hosts.
