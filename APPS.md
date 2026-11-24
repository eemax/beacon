# Building an app that reports well

**For whoever builds an app (a person or an AI agent): what to expose so the box can
report it through beacon, how to choose its levels and its numbers.** You don't need to
know beacon to follow this, and the app never depends on it. Point an agent here with
something like:

> Make this app observable following https://github.com/eemax/beacon/blob/main/APPS.md:
> a run record per run, a `status --json` command, clean exit codes, levels that mean what
> it says, the metrics it recommends.

## Where your app fits

People look at a hub (lookout) that should be safe to ignore until it matters. It sorts
everything into three lanes:

1. **Failing**: something people rely on is broken now. This reaches the owner (a page).
2. **Suggestions**: something works but should be looked at within days. A person or an
   agent picks it up; nobody is paged.
3. **Everything else**: numbers, history, lists. Shown and charted, never pushed at anyone.

Silence must mean nothing needs anyone, so what your app reports decides which lane it
lands in. Get the levels right (below) and the hub does the rest.

Each box runs **beacon**, which turns the box into **components** (the things you care
about: "Nightly sync", "Nightly backup"). Each one has a **class**, **runs** (every
execution of its job), **state** (what it is now, maybe with a list of **items**) and
**events** (deploys, reboots, changes, notes; each can say who made it and link to what
it's about). The hub draws every component the same way, from what the data says about
itself. There is no per-app dashboard code anywhere.

The work is split three ways:

| Who | Provides |
|---|---|
| **systemd + beacon** (free, nothing to build) | every run: start, end, duration, outcome (success, failure, stopped), exit code, what started it, the last output lines when it failed; the schedule and missed runs; reboots |
| **Your app** (this guide) | the facts only it knows, as plain JSON: what each run did, what it holds now, whether its parts are healthy, what's waiting |
| **The box's adapter** (a 20-line script next to beacon, written by whoever deploys the app) | turns your JSON into beacon's shape ([SPEC.md](SPEC.md)) |

So your app stays beacon-free. It exposes good facts in its own words; one small adapter
maps them. If you follow the shapes below, the adapter is little more than a rename.

## The two sentences

Every status your app reports (its own, or a check's) is held to these, word for word as
in SPEC.md:

> `fail` means what this component is for is not happening now; `warn` means it works, and someone should look within days; anything else is a metric.

- `fail`: the backup didn't happen, the site is down, the sync can't log in. Someone
  needs to act today.
- `warn`: it works, but an upstream is slow, a dependency is outdated, a certificate
  expires in two weeks, a run skipped 3 records. It can wait a few days.
- A number that is merely interesting (a size, a spend, a count of outdated packages) is
  a gauge, never a status. If nobody should act on it, don't make it a warning: a warning
  that sits for weeks teaches people to ignore the lane.

Ask, for each check: "if this fired at 3 am, should someone get up?" Only yes is `fail`.

## Pick a class

Whoever deploys the app gives its component a class in the definition (SPEC.md → Classes):

| Class | Use it for | Its `fail` |
|---|---|---|
| `service` (the default) | what people rely on: a job, a daemon, a backup, a site | is a failure: it pages |
| `advisory` | advice about the box: a doctor's findings, outdated dependencies, cost notes, coverage gaps | is only a suggestion: advice never pages |
| `list` | a list of things: work to do, things waiting on someone | as `advisory`; the list is its `items` |

Don't mix them. An app that does a job and also has advice to give (its dependencies are
old, it spends more than usual) is two components: the job as a `service`, the advice as
an `advisory`. Then the advice can say `fail` ("this is badly out of date") without paging
anyone, and the job's `fail` keeps meaning the job is broken.

## Items: work and waiting

When the useful answer is a list (changes to review, orders that need a fix, things
waiting on the owner), report **items**, not a count dressed up as a check. Each item:

```json
{ "id": "change-41", "title": "Raise the page size to 500", "since": "2026-10-20T09:12:00Z",
  "url": "https://code.example.com/changes/41", "waiting": true,
  "detail": "Approve or decline: it doubles the sync's speed and its memory." }
```

- `id` is stable (1-64 characters, unique in the list), so the hub can follow it from one
  state to the next; `title` says what it is in a line (at most 200).
- `waiting: true` when it waits on a person, the box's owner; leave it out otherwise.
- `status: "warn"` flags one; `url` is where to act on it; `since` when it began.
- At most 200. Items are never judged: they don't turn the component red or yellow. A
  count belongs in a gauge next to them (`items.open`) if it's worth charting.

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
   exit `0` for partial success and report the partial part (a `warn`). On SIGTERM, stop
   cleanly and exit (`143` is fine): that's recorded as *stopped*, not *failed*.
3. **Make the last line of output say why it failed**, as one plain sentence:
   `error: the ERP refused the login (HTTP 401): check ERP_PASSWORD`. That line is what
   people see in the alert. No stack trace as the last line; put it before.
4. **Write a run record** at the end of every run, success or failure (below).
5. **Offer a status command**, `<app> status --json` (below): cheap, read-only, fast.
6. **Levels by the two sentences**: `fail` only for "not happening now", `warn` for
   "look within days", the rest as numbers. Advice goes on an `advisory` component.
7. **Never put secrets or personal data** in output, records or status. Output tails end
   up on the hub. Put no tokens in URLs, command lines or error messages either.
8. **Keep names stable.** Keys, label values and item ids become history; renaming one
   starts a new series.
9. **Test the adapter's output with `beacon check --adapter`** (below), in the app's own
   tests and at deploy.

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
    { "key": "records.new", "label": "New", "value": 17, "unit": "count", "labels": { "endpoint": "orders" } }
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
"failed at endpoint invoices after 9 of 12" is far more useful than a bare exit code.

## The status command: what the app is now

`<app> status --json` prints the current state. Properties:

- **Read-only and fast** (well under 5 seconds; beacon calls it every minute). No network
  calls to upstreams in the default mode. Read your own database or state files.
- **Exit 0 even when unhealthy.** Unhealthy is data, not a crash. Reserve non-zero for
  "couldn't even look": beacon reads that as the component's **reporting broken**, not as
  the app down (below).
- Includes the last run record (`last_run`), so one command answers everything.

```json
{
  "healthy": false,
  "problems": ["invoices: no success since 2026-10-21T23:10:00Z (expected daily)"],
  "metrics": [
    { "key": "records.total", "label": "Records", "value": 365696, "unit": "count", "primary": true },
    { "key": "db.size", "label": "Database", "value": 1158201344, "unit": "bytes", "primary": true },
    { "key": "records.total", "label": "Records", "value": 30538, "unit": "count", "labels": { "endpoint": "orders" } }
  ],
  "checks": [
    { "name": "healthy", "status": "ok", "labels": { "endpoint": "orders" } },
    { "name": "healthy", "status": "fail", "detail": "no success since 2026-10-21T23:10:00Z", "labels": { "endpoint": "invoices" } }
  ],
  "items": [ { "id": "invoice-7", "title": "Invoice 7 has no VAT number", "waiting": true } ],
  "last_run": { "...": "the run record" }
}
```

## When reporting breaks

If the state adapter fails (it exits non-zero, times out, or prints a document beacon
refuses), beacon doesn't call the app down. It keeps the component's last good state,
keeps its runs, services and probes current, and adds `reporting` (the error, since when,
and when the adapter last worked). The hub reads that as "reporting broken": a suggestion
for an agent to fix the adapter, never a page about the app. So a change to the app that
breaks its status output costs a suggestion, not a night's sleep; and the deploy check
below keeps even that rare.

The same holds for a run adapter: a bad one never costs the run, which is logged as
systemd saw it, with a warning until the adapter works again.

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
| Spend | `spend.month` (`usd`) | a number to look at; over a budget is advice, on an `advisory` |

Box-level numbers (CPU, memory, disk) are beacon's built-in `host` component: skip them.

**Checks: yes/no questions with a reason.** Use a check, not a number, when the answer is
"is this OK?": an upstream reachable, credentials valid, each source fresh within its
expected age, a config sane. `status`: `ok`, `warn`, `fail`, by the two sentences;
`detail` says why in plain words ("no success since 2026-10-21T23:10:00Z, expected daily",
not "stale=true"). Freshness belongs here as a check with the threshold in your app, not
as a raw "age" number that rises all day.

**Times in text are timestamps, never ages.** A detail or summary is read minutes after it
was written (beacon collects every minute, the hub polls every few), so "heartbeat 28 s
ago" is wrong by the time anyone sees it. Write the time as RFC 3339 UTC, to the second:
`"last heartbeat 2026-10-23T06:58:12Z"`. The hub finds timestamps in text and shows them
like its own times, worked out when someone looks ("last heartbeat 2 min ago", the exact
time in their time zone on hover; UTC where it can't, like in a chat message).

**Headline numbers (`primary`).** At most 3 to 6 that answer "how did it go?" (for a
fetch: new and changed records; for a report: readiness and findings). They're shown on
the overview row and charted per run against the usual.

**Labels: "one of several things".** When a number is about one of several things (an
endpoint, a report, a queue, a table), give it labels (`{"endpoint": "orders"}`) instead
of baking the name into the key (`orders.records.new`). The hub turns labelled metrics
into a table: a row per thing, a column per key, plus the checks with the same labels as
a status column. Keep it bounded: dozens of rows, not thousands. Never use user ids,
request ids or timestamps as label values.

**Units: `count`, `percent` (0 to 100), `bytes`, `s`, `ms`, or a currency code (`usd`,
`eur`: ISO 4217, lowercase).** They decide formatting and axes everywhere ($58.56, <$0.01).
Report raw values in base units (bytes, not MB; seconds, not minutes; dollars, not cents).
Anything else is shown after the value as-is.

**Keys and labels.** Keys: lowercase, dotted, `thing.measure` (`records.new`, `db.size`),
at most 64 characters, `[a-z0-9._-]`. `label`: short, sentence case, without the app's
name ("New records", not "Sync new records"). Within one list, a key plus labels appears
once.

## What not to report

- Secrets, tokens, passwords, personal data: not in metrics, labels, items, summaries,
  errors or output.
- What systemd already knows: duration, start time, exit code, outcome.
- Per-request timings and anything that changes every second: that's a tracing or metrics
  system's job, not a status hub's.
- Unbounded label values (ids, names of things that come and go by the thousand).
- Warnings nobody acts on, and vanity numbers nobody would act on. Each number should
  answer a question someone would ask; each warning should be worth someone's look.

## Deploys

The installer that deploys your app (not the app itself) checks before it installs, and
logs a deploy event after:

```bash
# before the new version goes live: stage the new app, adapters and definitions in a
# directory (not /etc/beacon/components.d, whose adapters call the version still
# installed), with the definitions' commands pointing at the staged copy, and check that
beacon check --definitions "$STAGE/components.d" --only <id> || exit 1
# only then install: move $STAGE into place

# after: a deploy event, so changes line up with what happened on the timeline
if command -v beacon >/dev/null; then
  beacon event deploy --component <id> --title "<app> $(git rev-parse --short HEAD)" \
    --detail "$(git log -1 --format=%s)" --actor-kind agent --actor "<who deployed>" \
    --ref "Commit=commit:$(git rev-parse HEAD)" || true
fi
```

Check the staged copy, never the installed one: the installed definitions' adapters call
the old version, so they would pass whatever the new one breaks. `beacon check --definitions` runs each listed definition's state adapter and run adapter
once and checks what they print (a run adapter only reads, so that's safe), writing
nothing; it exits 1 on any bad one, so a deploy that would break a report is refused
there, before the new version goes live. Log the deploy event only when the version actually
changed. `--actor-kind person|agent|unit --actor <name>` says who deployed, `--ref` and
`--link "Changes=<url>"` what it points to, so a hub can show "deployed by …" next to what
broke after it, and a feed of who changed what. Other changes to the box's setup (a key
rotated, an access allowed, a rule edited) are `beacon event change`, with the same flags.

## Testing what you report

beacon checks every document against SPEC.md, and so can your tests: pipe what your
adapter prints into `beacon check`:

```bash
./adapters/myapp-state | beacon check --adapter -   # exit 0 valid, 1 with the reason
beacon check --definition components.d/myapp.json
```

A run adapter's output is checked as part of a run (`beacon check --run` on a run with its
output added, which `beacon check --definitions` does for you). Where beacon isn't
installed (a CI runner, a Mac), test against the shared cases in
[`testdata/contract/beacon-v3.json`](testdata/contract/beacon-v3.json): valid and invalid
documents of every kind, each invalid one with why.

## From app to hub: an example

A nightly sync (pulling 12 endpoints of an ERP into SQLite) exposes:

- exit codes that mean what they say;
- `sync status --json`: per endpoint `active_records`, the database path, and `last_run`
  (`started_at`, `finished_at`, `status`, `error`, `totals`, per-endpoint
  `inserted`/`updated`/`deleted`/`warning`);
- `sync check --json`: `healthy`, `problems`, per endpoint `healthy` + `problems`.

The box's adapters (about 60 lines each) map that to: per run, `records.new` and
`records.changed` (primary), `records.fetched`, and the same per `endpoint`; now,
`records.total` and `db.size` (primary), records per `endpoint`, a `healthy` check per
`endpoint` (`fail` when an endpoint has had no success in two days, `warn` after one
missed night). Its component is a `service`. Its outdated dependencies go on a separate
`advisory` component, so they're a suggestion and never a page. In lookout that becomes an
overview row ("succeeded 12 h ago · took 2m 5s · 697 new records · 927 changed records ·
next in 11 h"), a run history with the usual, and one "By endpoint" table with records,
fetched, new, changed, deleted and health per endpoint. None of it was designed per app.

## The adapter (for whoever deploys the app)

A definition in `/etc/beacon/components.d/<id>.json` names the class, the systemd units
and timers, and optionally two executables (SPEC.md → Adapters and definitions):

- **run adapter**: called once per finished run with `BEACON_RUN_STARTED_AT`,
  `BEACON_RUN_OUTCOME` etc. in the environment. It reads the run record, checks that
  `started_at` matches (within a minute or two), and prints `{"summary", "stats",
  "artifacts", "error"}`. Wrong record: print `{}`. It only reads: it may be run for a
  made-up run (a deploy check does), and must change nothing.
- **state adapter**: called every minute. It runs `status --json` and prints `{"status",
  "summary", "gauges", "checks", "links", "items"}`. Not `class` (the definition's) nor
  `reporting` (beacon's).

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

beacon checks what an adapter prints. A bad document never loses the run or turns the app
red: the run is logged without the adapter's part, and a bad state document is the
component's `reporting` until the adapter works again.
