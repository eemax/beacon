# Building an app that reports well

**For whoever builds an app (a person or an AI agent): what to expose so the box can
report it through beacon, and how to choose the numbers.** You don't need to know beacon
to follow this, and the app never depends on it. Point an agent here with something like:

> Make this app observable following https://github.com/eemax/beacon/blob/main/APPS.md:
> a run record per run, a `status --json` command, clean exit codes, the metrics it
> recommends.

## Where your app fits

People look at a hub (lookout) that answers, in this order:

1. Is anything wrong right now?
2. Did my jobs run, and what did they do? Was that normal?
3. How is the box?
4. How are things trending?
5. What happened, and when?
6. Where do I look next?

Each box runs **beacon**, which turns the box into **components** (the things you care
about: "Centric fetch", "Nightly backup"). Each one has **runs** (every execution of its
job), **state** (what it is now) and **events** (deploys, reboots, notes). The hub draws
every component the same way, from what the data says about itself. There is no per-app
dashboard code anywhere.

The work is split three ways:

| Who | Provides |
|---|---|
| **systemd + beacon** (free, nothing to build) | every run: start, end, duration, outcome (success, failure, stopped), exit code, what started it, the last output lines when it failed; the schedule and missed runs; reboots |
| **Your app** (this guide) | the facts only it knows, as plain JSON: what each run did, what it holds now, whether its parts are healthy |
| **The box's adapter** (a 20-line script next to beacon, written by whoever deploys the app) | turns your JSON into beacon's shape ([SPEC.md](SPEC.md)) |

So your app stays beacon-free. It exposes good facts in its own words; one small adapter
maps them. If you follow the shapes below, the adapter is little more than a rename.

## Software you didn't write

Caddy, a database, Authelia, cloudflared: nobody writes an app around those. beacon covers
them generically: name their systemd units in a definition and it reports each one's
state, uptime, restarts and memory, and logs crashes; a probe checks what clients get (the
status code, the response time, the TLS certificate as served). Only add an adapter when
the software has facts worth more than that (its own API or database); then this guide's
shapes apply to the adapter instead.

## The checklist

1. **Run as a systemd unit** (a `Type=oneshot` service plus a timer for scheduled jobs; a
   long-running service for daemons). Off systemd (cron, launchd), the box wraps it in
   `beacon exec`. Either way: don't build your own scheduler or "last run" bookkeeping
   for the hub's sake. Runs are captured from outside.
2. **Exit codes are the outcome.** `0` = it did its job. Non-zero = it didn't. Exit
   non-zero when the run failed to do what it's for, even if it "handled" the error;
   exit `0` for partial success and report the partial part (see warnings below). On
   SIGTERM, stop cleanly and exit (`143` is fine): that's recorded as *stopped*, not
   *failed*.
3. **Make the last line of output say why it failed**, as one plain sentence:
   `error: Centric API refused the login (HTTP 401): check CENTRIC_PASSWORD`. That line is
   what people see in the alert. No stack trace as the last line; put it before.
4. **Write a run record** at the end of every run, success or failure (below).
5. **Offer a status command**, `<app> status --json` (below): cheap, read-only, fast.
6. **Never put secrets or personal data** in output, records or status. Output tails end
   up on the hub. Put no tokens in URLs, command lines or error messages either.
7. **Keep names stable.** Keys and label values become history; renaming one starts a new
   series.

## The run record: what one run did

At the end of each run, write one JSON file atomically (a temp file, then rename), e.g.
`/var/lib/<app>/last-run.json`, or print it with `<app> status --json` as `last_run`:

```json
{
  "started_at": "2026-10-22T01:00:05Z",
  "finished_at": "2026-10-22T01:02:09Z",
  "status": "ok",
  "error": null,
  "summary": "delta fetch: 1,851 records, 697 new, 927 changed",
  "metrics": [
    { "key": "records.new", "label": "New records", "value": 697, "unit": "count", "primary": true },
    { "key": "records.changed", "label": "Changed records", "value": 927, "unit": "count", "primary": true },
    { "key": "records.new", "label": "New", "value": 17, "unit": "count", "labels": { "endpoint": "styles" } }
  ],
  "outputs": [ { "title": "Report", "url": "https://files.example.com/report.xlsx" } ]
}
```

- `started_at` (RFC 3339, UTC) is how the adapter matches the record to the run systemd
  saw. Set it when the run really starts.
- `status`: `ok`, `partial` or `failed`; `error`: one or two sentences when not ok.
- `summary`: one line a person would want in a chat message. Numbers formatted, no jargon.
- `metrics`: the numbers (below). The shape is beacon's own metric shape: copy it and the
  adapter passes them straight through. A domain-shaped record (`"totals": {"inserted":
  697}`) works too; then the adapter maps it.
- `outputs`: links to what the run produced, if people can open it.

Write it on failure too, with what's known (counts so far, `error`). A failed run with
"failed at endpoint boms after 9 of 12" is far more useful than a bare exit code.

## The status command: what the app is now

`<app> status --json` prints the current state. Properties:

- **Read-only and fast** (well under 5 seconds; beacon calls it every minute). No network
  calls to upstreams in the default mode. Read your own database or state files.
- **Exit 0 even when unhealthy.** Unhealthy is data, not a crash. Reserve non-zero for
  "couldn't even look".
- Includes the last run record (`last_run`), so one command answers everything.

```json
{
  "healthy": false,
  "problems": ["boms: no success since 2026-10-21 23:10 UTC (expected daily)"],
  "metrics": [
    { "key": "records.total", "label": "Records", "value": 365696, "unit": "count", "primary": true },
    { "key": "db.size", "label": "Database", "value": 1158201344, "unit": "bytes", "primary": true },
    { "key": "records.total", "label": "Records", "value": 30538, "unit": "count", "labels": { "endpoint": "styles" } }
  ],
  "checks": [
    { "name": "healthy", "status": "ok", "labels": { "endpoint": "styles" } },
    { "name": "healthy", "status": "fail", "detail": "no success since 2026-10-21 23:10 UTC", "labels": { "endpoint": "boms" } }
  ],
  "last_run": { "...": "the run record" }
}
```

## Choosing the numbers

Ask what a person would want to know the morning after, then pick at most a handful of
headline numbers and a breakdown. Duration, start time, outcome and exit code are
already captured: don't report them again.

**Per run (`metrics` in the run record): what the run did.**

| Kind | Examples | Why |
|---|---|---|
| Work done | `records.fetched`, `records.new`, `records.changed`, `records.deleted`, `files.written`, `emails.sent`, `bytes.uploaded` | the "what did it do" answer; drops to 0 or jumps 10× show up against the usual |
| Problems survived | `items.skipped`, `items.failed`, `retries`, `warnings` | partial success, visible without failing the run |
| The result itself | `readiness` (%), `findings`, `coverage` (%) | when the run computes something people care about |
| Phases (only if slow and uneven) | `phase.download` (s), `phase.load` (s) | which part got slow |

**Now (`metrics` in status): what the app holds.**

| Kind | Examples | Why |
|---|---|---|
| Size | `records.total`, `db.size` (bytes), `files.total` | growth, sudden drops (data loss) |
| Backlog | `queue.depth`, `pending`, `retry.backlog` | work piling up |
| Capacity it owns | `quota.used` (%), `disk.used` (bytes of its own folder) | running out |

Box-level numbers (CPU, memory, disk) are beacon's built-in `host` component: skip them.

**Checks: yes/no questions with a reason.** Use a check, not a number, when the answer is
"is this OK?": an upstream reachable, credentials valid, each source fresh within its
expected age, a config sane. `status`: `ok`, `warn`, `fail`; `detail` says why in plain
words ("no success since 2026-10-21 23:10 UTC, expected daily", not "stale=true").
Freshness belongs here as a check with the threshold in your app, not as a raw "age"
number that rises all day.

**Times in text are clock times, never ages.** A detail or summary is read minutes after
it was written (beacon collects every minute, the hub polls every few), so "heartbeat 28 s
ago" is wrong by the time anyone sees it. Write "last heartbeat 06:58 UTC" (or the date
too when it's not today); the hub shows ages itself, from timestamps it can trust.

**Headline numbers (`primary`).** At most 3 to 6 that answer "how did it go?" (for a
fetch: new and changed records; for a report: readiness and findings). They're shown on
the overview row and charted per run against the usual.

**Labels: "one of several things".** When a number is about one of several things (an
endpoint, a report, a queue, a table), give it labels (`{"endpoint": "styles"}`) instead
of baking the name into the key (`styles.records.new`). The hub turns labelled metrics
into a table: a row per thing, a column per key, plus the checks with the same labels as
a status column. Keep it bounded: dozens of rows, not thousands. Never use user ids,
request ids or timestamps as label values.

**Units: `count`, `percent` (0 to 100), `bytes`, `s`, `ms`, or a currency code (`usd`,
`eur`: ISO 4217, lowercase).** They decide formatting and axes everywhere ($58.56, <$0.01).
Report raw values in base units (bytes, not MB; seconds, not minutes; dollars, not cents).
Anything else is shown after the value as-is.

**Keys and labels.** Keys: lowercase, dotted, `thing.measure` (`records.new`, `db.size`),
at most 64 characters, `[a-z0-9._-]`. `label`: short, sentence case, without the app's
name ("New records", not "Centric new records"). Within one list, a key plus labels
appears once.

## What not to report

- Secrets, tokens, passwords, personal data: not in metrics, labels, summaries, errors or
  output.
- What systemd already knows: duration, start time, exit code, outcome.
- Per-request timings and anything that changes every second: that's a tracing or metrics
  system's job, not a status hub's.
- Unbounded label values (ids, names of things that come and go by the thousand).
- Vanity numbers nobody would act on. Each number should answer a question someone
  would ask.

## Deploys

The installer that deploys your app (not the app itself) logs a deploy event, so changes
line up with what happened on the timeline:

```bash
command -v beacon >/dev/null && beacon event deploy --component <id> \
  --title "<app> $(git rev-parse --short HEAD)" --detail "$(git log -1 --format=%s)" || true
```

Log it only when the version actually changed.

## From app to hub: an example

centric-api (a fetcher pulling 12 endpoints of a PLM system into SQLite, nightly) exposes:

- exit codes that mean what they say;
- `centric-api status --json`: per endpoint `active_records`, the database path, and
  `last_run` (`started_at`, `finished_at`, `status`, `error`, `totals`, per-endpoint
  `inserted`/`updated`/`deleted`/`warning`);
- `centric-api check --json`: `healthy`, `problems`, per endpoint `healthy` + `problems`.

The box's adapters (vps-hetzner `apps/beacon/adapters/`, about 60 lines each) map that
to: per run, `records.new` and `records.changed` (primary), `records.fetched`, and the
same per `endpoint`; now, `records.total` and `db.size` (primary), records per
`endpoint`, a `healthy` check per `endpoint`. In lookout that becomes an overview row
("succeeded 12 h ago · took 2m 5s · 697 new records · 927 changed records · next in 11 h"),
a run history with the usual, and one "By endpoint" table with records, fetched, new,
changed, deleted and health per endpoint. None of it was designed per app.

## The adapter (for whoever deploys the app)

A definition in `/etc/beacon/components.d/<id>.json` names the systemd units and timers,
and optionally two executables (beacon README → Describing a component):

- **run adapter**: called once per finished run with `BEACON_RUN_STARTED_AT`,
  `BEACON_RUN_OUTCOME` etc. in the environment. It reads the run record, checks that
  `started_at` matches (within a minute or two), and prints `{"summary", "stats",
  "artifacts", "error"}`. Wrong record: print `{}`.
- **state adapter**: called every minute. It runs `status --json` and prints `{"status",
  "summary", "gauges", "checks", "links"}`.

With the shapes above:

```python
#!/usr/bin/env python3
import json, os, subprocess
from datetime import datetime

def ts(s): return datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()

run = json.loads(subprocess.run(["myapp", "status", "--json"], capture_output=True, text=True, timeout=25).stdout).get("last_run") or {}
if not run.get("started_at") or abs(ts(run["started_at"]) - ts(os.environ["BEACON_RUN_STARTED_AT"])) > 120:
    print("{}"); raise SystemExit
print(json.dumps({"summary": run.get("summary") or "", "stats": run.get("metrics", [])[:100],
                  "artifacts": run.get("outputs", [])[:10], **({"error": run["error"]} if run.get("error") else {})}))
```

beacon checks what an adapter prints. A bad document never loses the run: it's logged
without the adapter's part, and the component shows a warning saying why.
