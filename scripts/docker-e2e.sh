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
#   one container runs `nfsserver --mount`, which drives mount.Mount
#   (in-process server + mount(2) syscall, needs CAP_SYS_ADMIN), and I/O is
#   checked through docker exec. The mount must sit at the requested path
#   itself, not behind a symlink.
#
# Scenarios B and C run with AppArmor unconfined: on hosts that enforce
# Docker's default AppArmor profile (Ubuntu, including GitHub's runners) it
# denies mount(2) even with CAP_SYS_ADMIN.
#
# Scenario C — a crashed server:
#   the mounting process is SIGKILLed; operations on the mount must fail
#   (soft mount) instead of hanging, and the next mount.Mount must detach the
#   stale mount (CleanupStale).
set -eu

IMG=simplefs-nfs
NET=simplefs-e2e
SERVER=simplefs-e2e-server
MOUNTER=simplefs-e2e-mounter
CRASHER=simplefs-e2e-crasher

MOUNTOPTS="--cap-add SYS_ADMIN --security-opt apparmor=unconfined"

cleanup() {
    status=$?
    if [ "$status" != 0 ]; then
        for c in "$SERVER" "$MOUNTER" "$CRASHER"; do
            docker logs "$c" >/dev/null 2>&1 || continue
            echo "-- logs of $c:"
            docker logs "$c" 2>&1 | tail -50
        done
    fi
    docker rm -f "$SERVER" "$MOUNTER" "$CRASHER" >/dev/null 2>&1 || true
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
docker run -d --name "$MOUNTER" $MOUNTOPTS "$IMG" --mount /mnt/simplefs >/dev/null
docker exec "$MOUNTER" sh -eu -c '
    for i in $(seq 1 20); do
        mountpoint -q /mnt/simplefs && break
        [ "$i" = 20 ] && { echo "mount did not appear"; exit 1; }
        sleep 0.5
    done
    [ ! -L /mnt/simplefs ] || { echo "/mnt/simplefs is a symlink, not the mount"; exit 1; }
    echo "in-container" > /mnt/simplefs/probe.txt
    [ "$(cat /mnt/simplefs/probe.txt)" = "in-container" ]
    mkdir /mnt/simplefs/dir
    mv /mnt/simplefs/probe.txt /mnt/simplefs/dir/
    [ "$(cat /mnt/simplefs/dir/probe.txt)" = "in-container" ]
    echo "scenario B: OK"
'

echo "== Scenario C: crashed server -> soft failure, then CleanupStale"
docker run -d --name "$CRASHER" $MOUNTOPTS --entrypoint sh "$IMG" -c 'sleep 3600' >/dev/null
docker exec -d "$CRASHER" nfsserver --mount /mnt/dead
docker exec "$CRASHER" sh -eu -c '
    for i in $(seq 1 20); do
        mountpoint -q /mnt/dead && break
        [ "$i" = 20 ] && { echo "mount did not appear"; exit 1; }
        sleep 0.5
    done
    echo alive > /mnt/dead/probe
    pkill -9 nfsserver
    sleep 1

    start=$(date +%s)
    set +e
    timeout -s KILL 90 ls /mnt/dead >/dev/null 2>&1
    status=$?
    set -e
    took=$(( $(date +%s) - start ))
    [ "$status" != 137 ] || { echo "ls on the dead mount hung"; exit 1; }
    echo "dead mount failed in ${took}s (status $status)"

    # A new mount cleans up the stale one.
    nfsserver --mount /mnt/fresh >/dev/null 2>&1 &
    for i in $(seq 1 20); do
        mountpoint -q /mnt/fresh && break
        [ "$i" = 20 ] && { echo "second mount did not appear"; exit 1; }
        sleep 0.5
    done
    if mountpoint -q /mnt/dead; then echo "stale mount not cleaned up"; exit 1; fi
    kill %1 2>/dev/null || pkill nfsserver
    echo "scenario C: OK"
'

echo "docker-e2e: ALL OK"
