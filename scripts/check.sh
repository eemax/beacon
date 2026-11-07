#!/bin/bash
# beacon's checks for `portal check` (.portal-checks.yaml): `scripts/check.sh fast|full`.
# What CI runs (gofmt, go vet, go test -race, bash -n install.sh), plus in `full` the Linux vet
# and the release cross-builds. Every test runs in temp dirs: no live systemd, journal or hub.
# The last line is the total that `count:` reads: tests passed, subtests included.
set -euo pipefail
cd "$(dirname "$0")/.."
mode=${1:-full}

unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then echo "gofmt: not formatted:"; echo "$unformatted"; exit 1; fi
go vet ./...
case $mode in
  fast) flags= ;;   # go's own test cache stays on: a working loop reruns only what changed
  full) flags="-race -count=1 -timeout 15m"
        # systemd and host code are per-OS: on the Mac, Linux's is only vetted and built here
        GOOS=linux go vet ./...
        bash -n install.sh
        # the release workflow's binaries, built and thrown away
        out=$(mktemp -d); trap 'rm -rf "$out"' EXIT
        for target in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64; do
          GOOS=${target%/*} GOARCH=${target#*/} CGO_ENABLED=0 go build -trimpath \
            -o "$out/beacon-${target%/*}-${target#*/}" ./cmd/beacon
        done
        echo "built: linux/amd64 linux/arm64 darwin/arm64 darwin/amd64" ;;
  *) echo "usage: scripts/check.sh fast|full" >&2; exit 2 ;;
esac

# -json for an exact count; the summary prints each package's line and a failing test's output.
set +e
go test $flags -json ./... | python3 -I -c '
import json, sys
out, passed, failed, skipped = {}, 0, [], []
for line in sys.stdin:
    try:
        e = json.loads(line)
    except ValueError:
        print(line, end="")  # build errors arrive as plain text
        continue
    key = (e.get("Package"), e.get("Test"))
    a = e.get("Action")
    if a == "output":
        out.setdefault(key, []).append(e.get("Output", ""))
    elif a in ("pass", "fail", "skip"):
        if e.get("Test"):
            if a == "pass":
                passed += 1
            elif a == "fail":
                failed.append(key)
                sys.stdout.write("".join(out.get(key, [])))
            else:
                skipped.append(key)
            out.pop(key, None)
        else:
            text = out.pop(key, [])
            summary = [l for l in text if l.startswith(("ok", "FAIL", "?", "---"))]
            if a == "fail" and not any(k[0] == key[0] for k in failed):
                sys.stdout.write("".join(text))  # a package failing outside a test (a panic, a build)
            else:
                sys.stdout.write("".join(summary) or f"{a} {key[0]}\n")
for k in skipped:
    print(f"SKIPPED {k[0]} {k[1]}")
for k in failed:
    print(f"FAILED {k[0]} {k[1]}")
print(f"check: {passed} tests passed" if not failed else f"check: {len(failed)} tests failed")
'
rc=$?
set -e
exit $rc
