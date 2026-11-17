# A containerized simplefs NFS server (examples/nfsserver backed by memfs).
#
#   docker build -t simplefs-nfs .
#   docker buildx build --platform linux/amd64,linux/arm64 -t simplefs-nfs .
#   docker run --rm -p 2049:2049 simplefs-nfs
#
# No privileges needed to SERVE. Consume the volume from outside:
#
#   # host (Linux)
#   sudo mount -t nfs -o vers=3,proto=tcp,mountproto=tcp,port=2049,mountport=2049,nolock 127.0.0.1:/ /mnt/simplefs
#   # host (macOS)
#   sudo mount_nfs -o vers=3,tcp,port=2049,mountport=2049,nolocks 127.0.0.1:/ /mnt/simplefs
#   # any other container, via Docker's built-in NFS volume driver
#   docker volume create -d local -o type=nfs \
#     -o o=addr=127.0.0.1,port=2049,mountport=2049,vers=3,tcp,nolock -o device=:/ simplefs
#   docker run --rm -v simplefs:/data alpine ls /data
#
# To mount INSIDE a container instead (and expose it through a shared bind
# mount), the container needs CAP_SYS_ADMIN:
#
#   docker run --rm --cap-add SYS_ADMIN simplefs-nfs --mount /mnt/simplefs

FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/nfsserver ./examples/nfsserver

FROM alpine:3.22
COPY --from=build /out/nfsserver /usr/local/bin/nfsserver
EXPOSE 2049/tcp 2049/udp 111/tcp 111/udp
ENTRYPOINT ["nfsserver"]
CMD ["--addr", ":2049"]
