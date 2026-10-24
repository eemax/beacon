# beacon

What's happening on a box, served read-only to a hub.

beacon knows the box's **components**: the things you care about on it (a nightly
integration, a report job, a backup, the host itself). For each it keeps the **state**
(gauges, checks, the schedule, a run in progress) and an append-only **log** of every
**run** (outcome, duration, exit code, the error, per-run stats) and **event** (reboots,
deploys, config changes, notes). A hub ([lookout](https://github.com/eemax/lookout)) polls
every box's beacon, keeps the history, draws the dashboard and sends the alerts. The box
needs no credentials for anything outside itself and accepts no writes from outside.

```
 box ──────────────────────────────────────────────────────────────────────┐
 │ systemd units, timers ──────┐                                            │
 │ adapters (components.d) ────┼─▶ beacon collect ─▶ components/*.json ──┐  │
 │ beacon exec (launchd, cron) ┘   (every minute)    log.jsonl ──────────┼─▶ beacon serve
 │ beacon event deploy|config|note ─────────────────────────────────────┘  │  (read-only)
 └──────────────────────────────────────────────────────────────────────────│─┘
                                                Cloudflare Tunnel (outbound only)
                                                                            │
                                                                         lookout
```

The data model and the HTTP API are in [SPEC.md](SPEC.md). **Building an app?**
[APPS.md](APPS.md) says what it should expose and which numbers to report (point your AI
agent at it).

## Describing a component

One file per component in `/etc/beacon/components.d/<id>.json`. For a job that runs as a
systemd unit, naming the unit is all it takes:

```json
{
  "id": "nightly-sync",
  "title": "Nightly sync",
  "group": "Integrations",
  "description": "Pulls yesterday's orders from the ERP.",
  "systemd": { "units": ["nightly-sync.service"], "timers": ["nightly-sync.timer"] },
  "schedule": { "every": "daily at 01:00", "grace": "1h" },
  "run": "/usr/local/lib/beacon/nightly-sync-run",
  "state": "/usr/local/lib/beacon/nightly-sync-state"
}
```

`beacon collect` (every minute) then logs **every invocation** of the unit as a run, with
no change to the job: start and end, outcome (`success`, `failure`, `cancelled` when it
was stopped), exit code, what started it (`timer`, `manual`, `boot`, `dependency`), and
the journal's last lines when it failed. While it runs, the component shows the run in
progress. The timers give the next run (`schedule.next_at`), so the hub sees a run that
didn't happen (`schedule.grace` after it was due).

| Field | |
|---|---|
| `id` | the file name without `.json`; `[a-z0-9][a-z0-9._-]*` |
| `title`, `group`, `description` | for the hub. `group` puts related components together |
| `systemd.units` | units whose invocations are runs of this component |
| `systemd.timers` | timers that start them: the schedule, and the `timer` trigger |
| `systemd.trigger` | trigger for runs no timer started: `manual` (default) or `dependency` (started by another unit) |
| `schedule.every` | how often it should run, in words, for people |
| `schedule.grace` | how late a run may be before it counts as missed (default `1h`) |
| `schedule.interval` | without timers: next run = last start + this |
| `expect_every` | how stale the component may get before the hub says so (default `10m`) |
| `probes` | checks as a client would see the service (see Services below) |
| `run` | **run adapter**: an executable run once per finished run (see below) |
| `state` | **state adapter**: an executable run every minute (see below) |
| `state_every` | run the state adapter at most this often (e.g. `5m`) and reuse its last result in between: for adapters that are slow or call an API. Runs, services and probes still update every minute |

### Adapters

Adapters are how a component says what only it knows. Each is an executable (no
arguments, run as root, 30 s limit) that prints one JSON object:

- the **run adapter** gets the finished run in its environment (`BEACON_COMPONENT`,
  `BEACON_RUN_ID`, `BEACON_RUN_UNIT`, `BEACON_RUN_OUTCOME`, `BEACON_RUN_EXIT_CODE`,
  `BEACON_RUN_STARTED_AT`, `BEACON_RUN_ENDED_AT`) and prints what that run did:
  `{"summary", "stats": [Metric], "artifacts": [Link], "error"}`;
- the **state adapter** prints what the component is now:
  `{"status", "summary", "gauges": [Metric], "checks": [Check], "links": [Link]}`.

Every field is optional. Shapes are in SPEC.md. beacon checks what adapters print: a
failing or invalid state adapter turns the component `fail` with the reason; a failing or
invalid run adapter never costs the run (it's logged as systemd saw it, and the component
warns until the adapter works again). A timer listed in the definition that isn't
scheduled (stopped, disabled) is a warning too, since without it nothing can be missed.

Adapters describe, the hub presents (SPEC.md → How a hub presents it): mark the headline
numbers `primary`, give metrics a `unit`, and put **labels** on metrics and checks that
are about one of several things (`endpoint=styles`): the hub turns those into a table
with a row per thing, without any display code anywhere.

### Services

`systemd.units` may also name long-running services (any unit that isn't `Type=oneshot`):
Caddy, a database, a daemon of your own. For each, the component gets an **active** check
and its uptime, automatic restarts and memory, labelled `unit=<name>` (a table in the
hub). When systemd restarts one after a crash, or it fails, beacon logs a `service` event
with the journal's last lines. Restarts you made (a deploy, `systemctl restart`) stay quiet:
log a deploy event for those.

**Probes** check a service the way a client would, from the box:

```json
{
  "id": "files",
  "title": "File store",
  "systemd": { "units": ["caddy.service", "dufs.service"] },
  "probes": [
    { "url": "https://files.example.com/", "expect": [200, 401] },
    { "name": "ssh", "tcp": "127.0.0.1:22" }
  ]
}
```

A `url` probe (`http://` or `https://`, redirects not followed) reports a **reachable**
check (the status must be in `expect`, default 200-399) and the response time; over HTTPS
also the **certificate** actually served: how long until it expires (warn under 14 days,
fail under 7) and whether clients would accept it. A `tcp` probe connects and reports
reachable and the time it took. Rows are labelled `probe=<name>` (default: the host). The
first probe's numbers are the component's headline. Every collect runs them (10 s limit
each).

### Jobs that aren't systemd units

Wrap them in `beacon exec`, from launchd, cron or by hand. It runs the command as it
would have run (output, exit code and signals pass through) and logs the run, with the
output's tail as the error when it fails (the run's `unit` is the command's name, never its
arguments):

```bash
beacon exec backup --trigger timer -- /usr/local/bin/backup.sh --full
```

A definition for `backup` is optional (add one for a title, a schedule
(`schedule.interval`) or adapters). A run whose process vanished is logged as failed.

### Events

Reboots are logged by `beacon collect` (a new boot id). Log the rest where they happen:

```bash
beacon event deploy --component nightly-sync --title "v1.4.2" --detail "fix retry backoff"
beacon event config --title "raised the API quota"
beacon event note --title "ERP down for maintenance until 06:00"
```

### The host

The built-in **`host`** component (Linux and macOS): load, memory, swap, disk, uptime,
pending reboot, with checks (warn at 85 % disk / 90 % memory, fail at 95 / 98).
`BEACON_HOST_FEED=off` turns it off.

## The box in short

`beacon brief` prints the box as a short Markdown summary for people and AI agents working
on it: each component's status with the reason (derived as the hub derives it, problems
first), its headline numbers, what's running, the next scheduled runs, and the last day's
runs and events (`--since 72h` for more). Times are RFC 3339 UTC. Point an agent's
instructions at it ("for the box's current state, run `beacon brief`") instead of pasting
status into its prompt.

## Install

### Linux (systemd)

```bash
git clone https://github.com/eemax/beacon /opt/beacon
/opt/beacon/install.sh --source <box-name> [--tunnel-token FILE --tunnel-address 10.249.x.y]
beacon token add lookout > /path/for/the/hub   # shown once
```

`install.sh` downloads the release binary (checksum-verified) for the box's architecture,
creates the `beacon` user, `/var/lib/beacon`, `/etc/beacon/{beacon.env,tokens,components.d}`
and the units, and starts them. Re-run it to update; it never touches tokens, definitions
or the log. `--binary ./beacon` installs a local build instead.

| Unit | What |
|---|---|
| `beacon.service` | `beacon serve` as user `beacon`, sandboxed (read-only filesystem, no capabilities) |
| `beacon-collect.timer` | `beacon collect` every minute, as root |
| `beacon-tunnel.service` | with `--tunnel-*`: cloudflared, outbound only, so the hub reaches the box with no open port; plus a drop-in that puts beacon on the tunnel address |

The tunnel is the only way in. The hub (a Cloudflare Worker) reaches beacon through the
box's tunnel over Workers VPC: one VPC Network binding, and per box one private `/32`
(from `10.249.0.0/16`) routed to that box's tunnel. No public hostname, no inbound port,
no DNS, works behind NAT. Cloudflare won't connect a Worker to loopback, so in tunnel mode
beacon listens on that private address, which exists only on the box (added to `lo`;
nothing routes to it from outside). On the hub, `lookout source tunnel <box>` creates the
tunnel, assigns the address and prints the token: pass them as `--tunnel-token` and
`--tunnel-address`.

### macOS (launchd)

No installer yet; launchd plays systemd's part. Put the release binary in
`/usr/local/bin/beacon`, set the tunnel address in `xyz.beacon.serve.plist`, then:

```bash
sudo mkdir -p /etc/beacon/components.d /var/lib/beacon
sudo cp deploy/launchd/*.plist /Library/LaunchDaemons/
sudo launchctl bootstrap system /Library/LaunchDaemons/xyz.beacon.collect.plist
sudo launchctl bootstrap system /Library/LaunchDaemons/xyz.beacon.serve.plist
sudo cloudflared service install "$(cat tunnel.token)"
```

Wrap launchd jobs in `beacon exec` (in their `ProgramArguments`) to log their runs.

## Commands

```
beacon collect [-v]                    update components and log new runs (every minute)
beacon exec <component> [--trigger T] -- <command> [args]
beacon event deploy|config|note --title T [--component C] [--detail D]
beacon run <component> --json FILE|-   log a finished run from a JSON document
beacon put <component> [flags]         write a component's state by hand
beacon serve                           serve state and log (read-only HTTP API)
beacon log [--after N] [--limit N]     print log entries
beacon brief [--since 24h]             the box in short, as Markdown (for people and AI agents)
beacon token add|list|revoke [NAME]
beacon validate
beacon version
```

Settings come from flags or the environment (`BEACON_DATA`, `BEACON_ETC`, `BEACON_LISTEN`,
`BEACON_SOURCE`, `BEACON_HOST_FEED`); the systemd units read `/etc/beacon/beacon.env`.
`beacon help` lists the defaults.

| Path | |
|---|---|
| `/etc/beacon/components.d/*.json` | definitions (root's) |
| `/etc/beacon/tokens` | SHA-256 hashes of the tokens that may read this box |
| `/var/lib/beacon/components/*.json` | each component's state |
| `/var/lib/beacon/log.jsonl` | runs and events (kept 90 days, at most 20,000 entries) |
| `/var/lib/beacon/state.json` | beacon's bookkeeping (seen invocations, runs in progress, boot id) |

## Security

- `serve` answers only `GET`/`HEAD` on `/healthz`, `/v2/state` and `/v2/log`, and reads
  only beacon's own files: regular files, no symlinks. Every request but `/healthz` needs
  a token; the tokens file stores only SHA-256 hashes.
- State and runs are status, not storage: never put secrets in them. A failed run's error
  is the journal's last lines, so keep secrets out of your jobs' output.
- Writing is local only (`collect`, `exec`, `event`, `run`, `put`). Adapters run as root,
  so only root can add definitions.

## Develop

```bash
go test -race ./...
go vet ./... && GOOS=linux go vet ./... && gofmt -l .
go build ./cmd/beacon
```

Releases: push a `vX.Y.Z` tag; CI builds linux/darwin × amd64/arm64 with `SHA256SUMS`.

## License

MIT, see [LICENSE](LICENSE).
