# beacon

Status feeds from a box, served read-only to a hub.

A box running beacon keeps a folder of small JSON **feeds**: how the box itself is doing,
how last night's integration went, how many new records it fetched, whether a backup
ran. A hub ([lookout](https://github.com/eemax/lookout)) polls every box's beacon, keeps
the history, draws the dashboard and sends the alerts. The box never needs credentials
for anything outside itself and accepts no writes from outside: it just keeps its folder
current.

```
 box ───────────────────────────────────────────────┐
 │ jobs ──beacon put──┐                              │
 │ collectors.d/* ────┼─▶ /var/lib/beacon/feeds/*.json ──▶ beacon serve (127.0.0.1:7600, read-only)
 │ host (built in) ───┘                              │            │
 └───────────────────────────────────────────────────┘     Cloudflare Tunnel (outbound only)
                                                                  │
                                                               lookout
```

The feed format and the HTTP API are in [SPEC.md](SPEC.md).

## Publishing a feed

At the end of a job:

```bash
beacon put centric-api --status ok --title "Centric fetch" --expect-every 26h \
  --summary "12 endpoints, 0 failing" \
  --metric styles.new=12:count:"New styles" --metric duration=58:s \
  --check "endpoints fresh=ok"
```

From a JSON document (anything that can print JSON): `producer | beacon put my-feed --json -`.
`schema`, `feed` and `updated_at` are filled in when missing.

On a timer: put an executable in `/etc/beacon/collectors.d/` that prints one feed
document. `beacon collect` (every minute, `beacon-collect.timer`) runs each one with a
30 s limit; the feed is named after the file (`centric.sh` → `centric`) unless the
document says otherwise. A collector that fails or prints garbage still produces its feed,
with status `fail` and the reason, so the hub sees the failure.

`beacon collect` also writes the built-in **`host`** feed on Linux: load, memory, swap,
disk, uptime, pending reboot (warn at 85 % disk / 90 % memory, fail at 95 / 98).

`expect_every` is what makes a dead producer visible: once a feed is older than that,
the hub shows it as stale, whatever its status says.

## Install (Linux, systemd)

```bash
git clone https://github.com/eemax/beacon /opt/beacon
/opt/beacon/install.sh --source <box-name> [--tunnel-token /path/to/token]
beacon token add lookout > /path/for/the/hub   # shown once
```

`install.sh` downloads the release binary (checksum-verified) for the box's architecture,
creates the `beacon` user, `/var/lib/beacon/feeds`, `/etc/beacon/{beacon.env,tokens,collectors.d}`
and the units, and starts them. Re-run it to update; it never touches feeds, tokens or
collectors. `--binary ./beacon` installs a local build instead.

| Unit | What |
|---|---|
| `beacon.service` | `beacon serve` as user `beacon`, sandboxed (read-only filesystem, no capabilities) |
| `beacon-collect.timer` | `beacon collect` every minute, as root |
| `beacon-tunnel.service` | with `--tunnel-token`: cloudflared, outbound only, so the hub reaches the box with no open port |

The tunnel is the only way in: beacon listens on `127.0.0.1` and the hub (a Cloudflare
Worker) reaches it through the tunnel over Workers VPC (a VPC Network binding and a
private hostname route per box). No public hostname, no inbound port, works behind NAT.
`lookout source add <name>` creates the tunnel and prints the token to install here.
(Status: the Workers VPC path is being verified; until then this section is the plan.)

macOS works too (no installer, no host feed): `beacon serve --feeds DIR --tokens FILE`
plus `cloudflared tunnel run --token-file FILE`.

## Commands

```
beacon put <feed> --status … | --json FILE|-
beacon collect
beacon serve
beacon token add|list|revoke [NAME]
beacon validate [FILE …]
beacon version
```

Settings come from flags or the environment (`BEACON_FEEDS`, `BEACON_TOKENS`,
`BEACON_COLLECTORS`, `BEACON_LISTEN`, `BEACON_SOURCE`, `BEACON_HOST_FEED`); the units read
`/etc/beacon/beacon.env`. `beacon help` lists the defaults.

## Security

- `serve` answers only `GET`/`HEAD`, only for the feeds folder: regular `*.json` files
  directly in it, no subfolders, no symlinks, nothing else on the box. Every request but
  `/healthz` needs a token; the tokens file stores only SHA-256 hashes.
- Feeds are status, not storage: never put secrets in one, private or not.
- Writing feeds is local only (`beacon put`, collectors), as root. Collectors run as root,
  so only root can add them.

## Develop

```bash
go test -race ./...
go vet ./... && gofmt -l .
go build ./cmd/beacon
```

Releases: push a `vX.Y.Z` tag; CI builds linux/darwin × amd64/arm64 with `SHA256SUMS`.
