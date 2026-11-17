#!/bin/sh
# Build every example — including those in nested modules — for the host
# and, with CGO_ENABLED=0, for linux, windows and darwin, so examples never
# rot.
#
#   sh scripts/build-examples.sh
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT

# "dir pattern": the root module's examples, then each nested module.
targets=". ./examples/..."
for mod in $(cd "$root" && find examples -name go.mod -exec dirname {} \; | sort); do
    targets="$targets $mod ./..."
done

fail=0
set -- $targets
while [ $# -gt 0 ]; do
    dir=$1 pattern=$2
    shift 2
    for os in host linux windows darwin; do
        printf '%-28s %-8s ... ' "$dir" "$os"
        if [ "$os" = host ]; then
            cmd="go build -o $out/ $pattern"
        else
            cmd="env GOOS=$os CGO_ENABLED=0 go build -o $out/ $pattern"
        fi
        if msg=$(cd "$root/$dir" && $cmd 2>&1); then
            echo ok
        else
            echo FAIL
            echo "$msg" | sed 's/^/    /'
            fail=1
        fi
    done
done
exit $fail
