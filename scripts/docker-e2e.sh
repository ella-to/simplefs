#!/bin/sh
# End-to-end test of the Linux NFS backend, entirely inside Docker.
#
# Scenario A — the "expose the volume outside" pattern:
#   an UNPRIVILEGED container serves the filesystem; a separate client
#   container (privileged, standing in for the host or any NFS consumer)
#   mounts it over the Docker network with the kernel NFS client and
#   exercises real file I/O through the mount.
#
# Scenario B — the in-container mount:
#   one container runs `nfsserver --mount`, which drives
#   mount.Mount/mount_linux.go (in-process server + mount(2) syscall,
#   needs CAP_SYS_ADMIN), and I/O is checked through docker exec.
set -eu

IMG=simplefs-nfs
NET=simplefs-e2e
SERVER=simplefs-e2e-server
MOUNTER=simplefs-e2e-mounter

cleanup() {
    docker rm -f "$SERVER" "$MOUNTER" >/dev/null 2>&1 || true
    docker network rm "$NET" >/dev/null 2>&1 || true
}
cleanup
trap cleanup EXIT

echo "== Scenario A: unprivileged server container + kernel-mount from a client container"
docker network create "$NET" >/dev/null
docker run -d --name "$SERVER" --network "$NET" "$IMG" >/dev/null

docker run --rm --privileged --network "$NET" alpine:3.22 sh -eu -c '
    apk add -q nfs-utils
    mkdir -p /mnt/simplefs
    for i in $(seq 1 20); do
        mount -t nfs -o vers=3,proto=tcp,mountproto=tcp,port=2049,mountport=2049,nolock \
            '"$SERVER"':/ /mnt/simplefs 2>/dev/null && break
        [ "$i" = 20 ] && { echo "mount failed"; exit 1; }
        sleep 0.5
    done
    cd /mnt/simplefs

    echo "hello from docker" > hello.txt
    [ "$(cat hello.txt)" = "hello from docker" ]

    mkdir -p docs/nested
    echo nested > docs/nested/file.txt
    [ "$(cat docs/nested/file.txt)" = "nested" ]

    mv hello.txt docs/renamed.txt
    [ "$(cat docs/renamed.txt)" = "hello from docker" ]
    [ ! -e hello.txt ]

    dd if=/dev/urandom of=big.bin bs=64k count=32 status=none
    md1=$(md5sum big.bin | cut -d" " -f1)
    cp big.bin copy.bin
    md2=$(md5sum copy.bin | cut -d" " -f1)
    [ "$md1" = "$md2" ]

    truncate -s 100 big.bin
    [ "$(stat -c %s big.bin)" = "100" ]

    ln -s docs/renamed.txt link.txt
    [ "$(cat link.txt)" = "hello from docker" ]

    rm -r docs copy.bin big.bin link.txt
    [ -z "$(ls -A)" ]

    cd /
    umount /mnt/simplefs
    echo "scenario A: OK"
'

echo "== Scenario B: in-container mount via mount.Mount (mount_linux.go)"
docker run -d --name "$MOUNTER" --cap-add SYS_ADMIN "$IMG" --mount /mnt/simplefs >/dev/null
docker exec "$MOUNTER" sh -eu -c '
    for i in $(seq 1 20); do
        [ -d /mnt/simplefs ] && mountpoint -q "$(readlink -f /mnt/simplefs)" && break
        [ "$i" = 20 ] && { echo "mount did not appear"; exit 1; }
        sleep 0.5
    done
    echo "in-container" > /mnt/simplefs/probe.txt
    [ "$(cat /mnt/simplefs/probe.txt)" = "in-container" ]
    mkdir /mnt/simplefs/dir
    mv /mnt/simplefs/probe.txt /mnt/simplefs/dir/
    [ "$(cat /mnt/simplefs/dir/probe.txt)" = "in-container" ]
    echo "scenario B: OK"
'

echo "docker-e2e: ALL OK"
