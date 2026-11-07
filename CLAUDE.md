# beacon

Go, stdlib only. Components, runs and events on a box (SPEC.md), served read-only to the lookout hub.
README.md has the overview; APPS.md is the guide for apps (what to expose); keep it in
step with SPEC.md and lookout's display rules.

- Develop and release from a checkout; boxes install release binaries
  (`install.sh`), never by building on the box.
- SPEC.md is the contract with [lookout](https://github.com/eemax/lookout). Changing the format or the API means
  changing both, and SPEC.md first. Additive optional fields stay in the same schema.
- `serve` must stay read-only and confined to beacon's own files (components/, the log, tokens):
  regular files, no symlinks. Anything that widens what it can read or lets it write is a design change: ask.
- No third-party dependencies without a good reason; it has to drop onto any box.
- Before committing: `gofmt -l .` (empty), `go vet ./...`, `go test -race ./...`; also
  `GOOS=linux go vet ./...` (systemd and host code are per-OS).
- Checks: `~/portal/bin/portal check fast` while working, `portal check full` before a build
  reports (`.portal-checks.yaml`, `scripts/check.sh`: the list above plus the release cross-builds).
- Release: tag `vX.Y.Z` and push the tag; the release workflow builds the binaries. Tag only
  when a box should get the change; bump the last number by default, the middle one for a named
  feature, the first only for a breaking change and only on Max's say-so (his preferences,
  Building → Versions).
- Test systemd capture on a real box (ysera): cross-compile, copy to /tmp, run with
  `BEACON_DATA`/`BEACON_ETC` pointing at a temp dir, clean up after.
