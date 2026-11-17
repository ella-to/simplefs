#!/bin/sh
# Vet every package for every supported GOOS/GOARCH, in the root module and in
# each nested module (any directory below the root holding a go.mod).
#
#   sh scripts/cross-vet.sh
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)

# target = GOOS/GOARCH/CGO_ENABLED
targets="
linux/amd64/0
linux/arm64/0
windows/amd64/0
windows/arm64/0
darwin/arm64/0
freebsd/amd64/0
"
# cgo for darwin only works on a darwin host (needs the SDK).
if [ "$(uname -s)" = Darwin ]; then
    targets="$targets darwin/arm64/1"
fi

modules=$(cd "$root" && find . -name go.mod -not -path './build/*' -exec dirname {} \; | sort)

fail=0
for mod in $modules; do
    for t in $targets; do
        goos=${t%%/*}; rest=${t#*/}; goarch=${rest%%/*}; cgo=${rest#*/}
        printf '%-28s %-16s cgo=%s ... ' "$mod" "$goos/$goarch" "$cgo"
        if out=$(cd "$root/$mod" && GOOS=$goos GOARCH=$goarch CGO_ENABLED=$cgo go vet ./... 2>&1); then
            echo ok
        else
            echo FAIL
            echo "$out" | sed 's/^/    /'
            fail=1
        fi
    done
done
exit $fail
