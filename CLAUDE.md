# beacon

Go, stdlib only. Status feeds on a box (SPEC.md), served read-only to the lookout hub.
README.md has the overview.

- Develop and release from a checkout; boxes install release binaries
  (`install.sh`), never by building on the box.
- SPEC.md is the contract with [lookout](https://github.com/eemax/lookout). Changing the format or the API means
  changing both, and SPEC.md first. Additive optional fields stay schema 1.
- `serve` must stay read-only and confined to regular `*.json` files directly in the feeds
  folder. Anything that widens what it can read or lets it write is a design change: ask.
- No third-party dependencies without a good reason; it has to drop onto any box.
- Before committing: `gofmt -l .` (empty), `go vet ./...`, `go test -race ./...`; also
  `GOOS=linux go vet ./...` (the host collector is Linux-only).
- Release: tag `vX.Y.Z` and push the tag; the release workflow builds the binaries.
