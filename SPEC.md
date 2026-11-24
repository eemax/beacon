# beacon v3: what a box tells its hub

A **box** runs beacon. beacon knows the box's **components** (the things you care about on
it: a nightly integration, a report job, a backup, the host itself) and keeps:

- **state**: what each component is now (gauges, checks, items, a schedule, a run in
  progress), a snapshot that's overwritten;
- **a log** of **runs** (every execution of a component's job) and **events** (reboots,
  deploys, config changes, other changes, notes), append-only.

A hub ([lookout](https://github.com/eemax/lookout)) reads both over beacon's read-only HTTP
API: the state on every poll, the log from where it left off, so it sees every run exactly
once. The design behind this is lookout's `docs/design.md`.

For people and agents building apps: [APPS.md](APPS.md) (what to expose, which numbers).

**Producers describe, hubs present.** Nothing here says how to draw anything; it says what
things are (a class, units, labels, headline numbers, outcomes, schedules, items), and the
hub derives the display and what reaches a person from that, the same way for every
component.

## The levels

A status (a component's own, or a check's) is one of `ok`, `warn`, `fail`, `unknown`.
Two of them carry a promise, the same for every component:

> `fail` means what this component is for is not happening now; `warn` means it works, and someone should look within days; anything else is a metric.

So a producer marks `fail` only when the thing people rely on is broken right now (the
backup didn't happen, the site is down), and `warn` for what can wait a few days (an
outdated dependency, a certificate expiring in two weeks, a cost over its usual). A number
that is merely interesting (a size, a count, a spend) is a gauge, never a status. `unknown`
means it couldn't tell; a hub shows it as it is.

## Classes

Every component has a **class**, which says what its levels mean to a hub:

| Class | What it is | Leads to a failure (the owner is told) | Leads to a suggestion (someone looks within days) |
|---|---|---|---|
| `service` (the default) | something people rely on: a job, a service, a backup, the host | a `fail` status or check · the latest run failed (until a later run succeeds) · a missed run · stale state | every `warn` · a cancelled run · `reporting` |
| `advisory` | advice about the box: a doctor's findings, outdated software, a cost note, a coverage gap | never | anything not ok: a `fail` or `warn` status or check, a failed, cancelled or missed run, stale state, `reporting` |
| `list` | a list of things (work to do, things waiting on someone), held in `items` | never | as `advisory` |

Items are never judged: they're shown as a list on any class, and the ones marked
`waiting` are what waits on the box's owner. The class is the producer's to say (in a
definition, or in what it writes through `beacon put`); left out, it's `service`.

## Common shapes

**Metric** (a gauge of the state, or a stat of a run):

| Field | Required | Rules |
|---|---|---|
| `key` | yes | `[a-z0-9][a-z0-9._-]*`, at most 64 characters |
| `value` | yes | a finite number |
| `label` | no | at most 100; sentence case, without the component's name (`New records`). Hubs fall back to the key |
| `unit` | no | at most 16: `count`, `percent` (0-100), `bytes`, `s`, `ms`, a currency code (`usd`, ISO 4217), or anything else (shown after the value) |
| `labels` | no | at most 4 `{"name": "value"}`: which of several things it's about (`{"endpoint": "orders"}`). Names like keys, values 1-64 characters without `, = { }` |
| `primary` | no | `true` for a headline number (at most 6 per list) |

A metric's identity is its key plus its labels (`records.new{endpoint=orders}`); each may
appear once per list.

**Check:** `name` (1-100), `status` (`ok`, `warn`, `fail`, `unknown`), optional `detail`
(at most 500) and `labels`.

**Link:** `title` (1-100) and `url` (`http(s)://` with a host, at most 500).

**Item** (one thing in a component's list):

| Field | Required | Rules |
|---|---|---|
| `id` | yes | 1-64 code points with no control characters; unique within the component; stable, so a hub can follow the item from one state to the next |
| `title` | yes | 1-200 |
| `since` | no | when it began (RFC 3339) |
| `status` | no | `ok` or `warn` (`warn` = flagged) |
| `url` | no | where to act on it, by the Link rules (`http(s)://` with a host, at most 500) |
| `waiting` | no | `true`: it waits on a person, the box's owner (leave it out otherwise; `false` is refused) |
| `detail` | no | at most 500 |

**Lengths** ("at most 100") count Unicode code points, not bytes. A document beacon reads
(a component, an adapter's output, a run, a definition) is at most 256 KiB.

**Times** are RFC 3339 strings; **durations** are like `90s`, `5m`, `26h`, `1h30m`. A time
inside free text (a summary, a check's detail, an error) is also written as RFC 3339 UTC
(`2026-10-23T06:58:12Z`), never as an age: hubs show it like their own times ("2 min ago",
the exact time in the reader's time zone on hover).

**Never put secrets in anything beacon serves.**

## Component (state)

```json
{
  "id": "nightly-sync",
  "class": "service",
  "title": "Nightly sync",
  "group": "Integrations",
  "description": "Pulls yesterday's orders from the ERP into SQLite.",
  "updated_at": "2026-10-22T11:02:00Z",
  "expect_every": "10m",
  "schedule": { "next_at": "2026-10-23T01:00:00Z", "grace": "1h", "every": "daily at 01:00 UTC" },
  "running": { "id": "4f1c…", "started_at": "2026-10-22T01:00:02Z", "trigger": "timer" },
  "status": "ok",
  "summary": "12 endpoints, 364,972 records",
  "gauges": [ { "key": "records", "label": "Records", "value": 30538, "unit": "count", "labels": { "endpoint": "orders" } } ],
  "checks": [ { "name": "healthy", "status": "ok", "labels": { "endpoint": "orders" } } ],
  "links": [ { "title": "Files", "url": "https://example.com/files/" } ],
  "items": [ { "id": "order-1042", "title": "Order 1042 has no customer", "since": "2026-10-21T08:00:00Z", "status": "warn", "waiting": true } ]
}
```

| Field | Required | Meaning |
|---|---|---|
| `id` | yes | `[a-z0-9][a-z0-9._-]*`, at most 64; stable |
| `class` | served: yes; written: no | `service`, `advisory` or `list` (Classes above). What a producer writes may leave it out (`service`); beacon always serves it |
| `title` | no | display name (at most 100) |
| `group` | no | a heading to group components under (at most 50), e.g. `Integrations`, `System` |
| `description` | no | one sentence: what it is (at most 300) |
| `updated_at` | yes | when this state was written |
| `expect_every` | no | longest normal gap between state updates; past it the state is **stale** |
| `schedule` | no | for components that run on a schedule: `next_at` (the next expected start), `grace` (how late a start may be before it counts as **missed**, default `1h`), `every` (human description, at most 100) |
| `running` | no | a run in progress: `id`, `started_at` (required), `trigger` |
| `status`, `summary` | no | the producer's own verdict (The levels) and one line (at most 500) about the state |
| `gauges` | no | at most 100 metrics: values now |
| `checks` | no | at most 50 |
| `links` | no | at most 10 |
| `items` | no | at most 200, on any class |
| `reporting` | served only | written only by beacon, below |

**`reporting`** says the component's own reporting is broken, not the component:

```json
"reporting": { "error": "state adapter failed: exit status 1: database is locked",
               "since": "2026-10-22T10:41:00Z", "last_good_at": "2026-10-22T10:40:00Z" }
```

| Field | Required | Meaning |
|---|---|---|
| `error` | yes | why, 1-500 |
| `since` | yes | when it first failed, this time |
| `last_good_at` | yes | when the adapter last printed a valid document, or `null` if it never has |

It is present only while the component's state adapter fails (an error, a timeout, a
non-zero exit) or prints an invalid document, and gone at its next valid one. Meanwhile the
rest of the component is the adapter's last good output (`status`, `summary`, its gauges,
checks, links and items; none if there was none), and beacon's own parts stay current: the
runs, `schedule`, `running`, the services' and the probes' gauges and checks. A hub reads
`reporting` as "reporting broken" (a suggestion), never as the component failing.

beacon adds its own check `beacon` (`warn`) while a run was logged without its run
adapter's part or a timer in the definition isn't scheduled, with the reason in its detail.

## Log entries

The log's entries are as in schema 2. Every entry has `seq` (a positive integer, increasing
by one per entry on this box, never reused) and `type`.

**Run** (written when the run ends):

```json
{
  "seq": 812, "type": "run", "component": "nightly-sync", "id": "4f1c…",
  "started_at": "2026-10-22T01:00:02Z", "ended_at": "2026-10-22T01:02:06Z",
  "outcome": "success", "trigger": "timer", "exit_code": 0, "unit": "nightly-sync.service",
  "summary": "delta fetch: 1,851 records, 697 new, 927 changed",
  "stats": [
    { "key": "records.new", "label": "New records", "value": 697, "unit": "count", "primary": true },
    { "key": "records.new", "label": "New", "value": 17, "unit": "count", "labels": { "endpoint": "orders" } }
  ],
  "artifacts": [ { "title": "Report", "url": "https://example.com/report.xlsx" } ]
}
```

| Field | Required | Meaning |
|---|---|---|
| `type` | in the log | `run` |
| `seq` | in the log | the entry's place in the log |
| `component` | in the log | the component's `id` (`beacon run` takes it from its command line) |
| `id` | in the log | unique per box (systemd's InvocationID, or a random id): `[A-Za-z0-9][A-Za-z0-9._:-]*`, at most 64 (`beacon run` makes one up when it's missing) |
| `started_at`, `ended_at` | yes | `ended_at` not before `started_at` |
| `outcome` | yes | `success`, `failure` or `cancelled` (stopped from outside) |
| `trigger` | no | `timer`, `manual`, `boot`, `dependency`, `other` |
| `exit_code` | no | the process's exit status, an integer |
| `unit` | no | the systemd unit or command that ran (at most 200) |
| `summary` | no | one line (at most 500) |
| `stats` | no | at most 100 metrics: what the run produced |
| `error` | no | why it failed, at most 2000 characters (the end of its output if nothing better) |
| `artifacts` | no | at most 10 links: what it made |

**Event:**

```json
{ "seq": 813, "type": "event", "id": "b7…", "at": "2026-10-22T00:00:41Z", "kind": "reboot",
  "component": null, "title": "Rebooted", "detail": "kernel 6.8.0-146 → 6.8.0-147" }
{ "seq": 814, "type": "event", "id": "c9…", "at": "2026-10-22T09:14:03Z", "kind": "deploy",
  "component": "nightly-sync", "title": "nightly-sync 4e1a2b7", "detail": "retry on 429",
  "actor": { "kind": "agent", "name": "deploy-bot" },
  "links": [ { "title": "Commit", "ref": { "kind": "commit", "id": "4e1a2b7" } },
             { "title": "Changes", "url": "https://example.com/compare/3f0c9d1...4e1a2b7" } ] }
```

| Field | Required | Meaning |
|---|---|---|
| `id` | yes | unique per box, at most 64 |
| `at` | yes | |
| `kind` | yes | `reboot`, `deploy`, `config`, `note`, `service` (a long-running service crashed, or failed), `change` (a change to the box's setup that isn't a deploy, a config change or a service event: a key rotated, an access allowed, a rule edited) |
| `component` | no | the component it's about, or null for the whole box |
| `title` | yes | at most 200 |
| `detail` | no | at most 2000 |
| `actor` | no | who made the change: `kind` (`person` a human, `agent` an AI agent or automation acting for someone, `unit` a systemd unit or job) and `name` (at most 100). Leave it out when nobody did (a reboot, a crash) or nobody knows |
| `links` | no | at most 10: what the event points to, each with a `title` (at most 100) and exactly one of `url` (`http(s)://` with a host, at most 500) or `ref` (`kind`: `commit`, `alert`, `incident`, `conversation` or `other`; `id`: at most 200), for things that live elsewhere and have no URL of their own |

A ref names a thing in the producer's world (a commit hash, a hub's alert or incident id, an
agent's conversation); a hub that knows where such things live may turn it into a link, and
shows it as text otherwise. An event kind is always a token, `[a-z][a-z0-9-]{0,31}`: a hub
accepts any such token and shows an unknown one as it is, while beacon writes only the
kinds listed here.

The box keeps the log for 90 days (at most 20,000 entries).

## Adapters and definitions

A **definition** (`/etc/beacon/components.d/<id>.json`, root's) says what a component is
and where its data comes from:

| Field | Required | Rules |
|---|---|---|
| `id` | yes | a component id, the file's name without `.json`; not `host` |
| `class` | no | `service` (the default), `advisory` or `list` |
| `title`, `group`, `description`, `expect_every` | no | as in the component |
| `systemd` | no | `units` (required, at least one), `timers`, `trigger` (`manual`, the default, or `dependency`, for runs no timer started) |
| `schedule` | no | `every` (words, at most 100), `grace` (a duration), `interval` (a duration: without timers, the next run is the last start plus this) |
| `probes` | no | at most 10, each a `url` (`http(s)://`, no credentials; `expect`: HTTP statuses that count as up, default 200-399) or a `tcp` (`host:port`), with an optional `name` (1-64 characters without `, = { }`, unique) |
| `state` | no | the **state adapter**: an executable, run every collect |
| `state_every` | no | a duration: run the state adapter at most this often (needs `state`) |
| `run` | no | the **run adapter**: an executable, run once per finished run |

The **state adapter** prints one JSON object, what the component is now: `{"status",
"summary", "gauges", "checks", "links", "items"}`, each optional, by the component's rules.
`class` and `reporting` are invalid there (the class is the definition's, reporting is
beacon's), as is any other field.

The **run adapter** gets the finished run in its environment (`BEACON_COMPONENT`,
`BEACON_RUN_ID`, `BEACON_RUN_UNIT`, `BEACON_RUN_OUTCOME`, `BEACON_RUN_EXIT_CODE`,
`BEACON_RUN_STARTED_AT`, `BEACON_RUN_ENDED_AT`) and prints what that run did: `{"summary",
"stats", "artifacts", "error"}`, each optional (`{}` when it has nothing for this run).
beacon adds that to the run systemd saw, and the result is checked as a run. **A run adapter
only reads**: it may be run again for the same run, or for a made-up one, without harm (so a
check may run it once).

Every adapter runs with no arguments, as root, within 30 s. What it prints is checked: a bad
state adapter's document is the component's `reporting`; a bad run adapter's never costs
the run (it's logged as systemd saw it, and the `beacon` check warns until the adapter
works again).

## Documents and how to check one

Each document beacon reads or serves is of one **kind**, and `beacon check` validates one
document of a kind:

| Kind | What it is | Flag | Read by a hub | Written by a producer |
|---|---|---|---|---|
| envelope | a whole `/v3/state` answer | `--envelope` | yes | |
| served | a component as a hub is served it: `class` required, `reporting` allowed | `--served` | yes | |
| component | what a producer writes through `beacon put`: `class` optional, `reporting` refused; `id` and `updated_at` may be left to `beacon put` (the command's id, the time now) | `--component` | | yes |
| adapter | a state adapter's output | `--adapter` | | yes |
| run | a run: a log entry, a run adapter's output added to the run it was given, or `beacon run --json`'s document | `--run` | yes | yes |
| definition | a `components.d` file | `--definition` | | yes |

```
beacon check --envelope|--served|--component|--adapter|--run|--definition FILE|-
beacon check --definitions DIR [--only ID,...]
```

`beacon check --<kind> FILE|-` reads the document from FILE (or stdin for `-`), exits 0 when
it is valid and 1 when it isn't, printing the reason. For `--definition` with a FILE, the id
must also match the file's name. A run may leave out `type`, `seq`, `component` and `id`
(what `beacon run` takes; it fills them in); every entry in the log has them.

`beacon check --definitions DIR [--only ID,...]` checks each definition in DIR (only the
listed ids with `--only`): the file as a definition, then it runs its state adapter once and
checks what it prints as adapter output, and its run adapter once, for a made-up successful
run that started and ended now, and checks the result as a run. It prints `ok <id>` or
`BAD <id>: <why>` per definition, writes nothing (no component, no log entry), and exits 1
on any BAD or an `--only` id with no definition. An app runs `beacon check --adapter` in its
own tests and the box runs `beacon check --definitions` at deploy, so a change that would
break a report is refused before it's installed. The check has to run against the staged new
definitions, adapters and app (a DIR holding them, whose commands call the staged app), not
the installed ones: those call the old version and would pass whatever the new one breaks.

Unknown fields are invalid in every kind beacon checks, so a typo fails loudly. A hub
ignores fields it doesn't know in what it reads (envelope, served, run), so a new optional
field can appear within schema 3 (Versioning).

## The built-in host component

Every box has a component `host` (class `service`, group `System`; Linux and macOS) for the
box itself. Its gauges are load, memory, swap, disk and uptime, plus on Linux
`units.failed` and `oom.kills` (since boot). Its checks, each read cheaply on every collect
(once a minute):

| Check | Where | ok | warn | fail |
|---|---|---|---|---|
| `disk` | all | below 85 % used | from 85 % | from 95 % |
| `memory` | all | below 90 % used | from 90 % | from 98 % |
| `units` | Linux (systemd) | no failed unit | a failed unit no component watches, named in the detail | — |
| `oom` | Linux | no kernel OOM kill in the last 24 h | one or more in the last 24 h: how many, when the last was seen, what was killed when the kernel log says | — |
| `reboot` | Linux (`/var/run/reboot-required`) | none pending, or pending for fewer than 3 nights | pending through 3 midnights (UTC) or more: the box's own reboots aren't happening | — |

`units` leaves out the units a component's definition names (`systemd.units`,
`systemd.timers`): their component already shows them failing. It also leaves out the
transient units `systemd-run` makes (`run-*.service` and `run-*.scope`): they belong to
whoever ran them, not to the box. A reboot's `detail` gives when it became pending (RFC
3339) and the packages asking for it.

## HTTP API

`beacon serve` listens on `127.0.0.1:7600` (in tunnel mode on the box's private tunnel
address). Every request except `/healthz` needs `Authorization: Bearer <token>`; only
`GET`/`HEAD` are answered.

**`GET /v3/state`** (with `ETag`; `If-None-Match` → `304`), an envelope:

```json
{
  "schema": 3,
  "source": "box-1",
  "beacon": "v3.0.0",
  "generated_at": "2026-10-22T11:02:10Z",
  "box": { "os": "Ubuntu 24.04.5 LTS", "kernel": "6.8.0-147-generic", "arch": "amd64",
           "boot_at": "2026-10-22T00:00:30Z" },
  "log": { "first_seq": 1, "last_seq": 813 },
  "components": [ { "id": "host", "class": "service", "...": "..." } ],
  "invalid": [ { "file": "broken.json", "error": "…" } ]
}
```

| Field | Required | Meaning |
|---|---|---|
| `schema` | yes | `3` |
| `source` | yes | the box's name, as the hub knows it (1-64) |
| `beacon` | yes | beacon's version (1-64) |
| `generated_at` | yes | when this answer was made |
| `box` | yes | `os`, `kernel`, `arch` (each optional), `boot_at` (optional) |
| `log` | yes | `first_seq` and `last_seq`: the log's bounds (0 and 0 while it's empty) |
| `components` | yes | served components, each id once |
| `invalid` | yes | files beacon couldn't read as a component or definition: `file` and `error` |

**`GET /v3/log?after=<seq>&limit=<n>`**: entries with `seq > after`, oldest first, at most
`limit` (default and maximum 500):

```json
{ "entries": [ { "seq": 812, "type": "run", "...": "..." } ], "first_seq": 1, "last_seq": 813 }
```

A hub keeps the last `seq` it stored and asks for more while `last_seq` is ahead. If its
`after` is older than `first_seq - 1`, entries were pruned before it saw them (a gap it
can note).

`GET /healthz` → `ok`. Anything else is `404`, `/v2/*` and `/v1/*` included.

## How a hub presents it (what producers can rely on)

- **Lanes**: what a component's class and levels lead to (Classes): a **failure** reaches
  the owner; a **suggestion** waits for someone (a person or an agent) to pick it up within
  days; everything else is shown and charted, and never reaches anyone. Each is shown with
  its reason.
- **Reporting broken**: a component's `reporting` reads as its reporting broken, a
  suggestion, never as the component failing; its runs, services and probes are still
  judged. An answer that isn't schema 3 reads as the whole source's reporting broken.
- **Status per component**, worst of: latest run failed (until a later run succeeds) ·
  **missed** (`schedule.next_at + grace` passed with no run started) · a check failing or
  warning · **stale** state · the producer's `status`. Always shown with its reason.
- **Items** as a list per component, never judged; the `waiting` ones as what waits on the
  owner, `warn` ones flagged, each with its link.
- **Headline numbers**: `primary` metrics (stats of the latest run first, then gauges), as
  tiles; with none marked, the first unlabelled ones.
- **Tables from labels**: metrics with labels group by label *names* into a table: a row per
  label value, a column per key (header = its `label`); checks with the same labels become
  that table's status column.
- **History**: run stats chart one point per run, compared with the median of earlier runs
  ("3× usual"); gauges chart over time.
- **Units** decide formatting everywhere; `links` and `artifacts` become buttons.
- **Events** go on the timeline with their actor ("deploy by agent deploy-bot"); their links
  are clickable (a ref where the hub knows where it points, else its text). Events can be
  filtered by kind and by actor, so "what changed in the 2 h before this broke" and "what
  did this agent do" are one filter each.
- **Group** headings order the components; within a box, problems first.

## Versioning

`schema` changes only when a reader would break. New optional fields, and new values of an
enumeration such as an event's `kind`, may appear within schema 3: a hub shows a kind it
doesn't know as it is, and ignores fields it doesn't know.

Schema 3 since beacon v3.0.0: classes, items, `reporting`, the document kinds and
`beacon check`, under `/v3/state` and `/v3/log`. It replaced schema 2 (`/v2/state`,
`/v2/log`) outright: no hub reads 2, and beacon v3 serves only `/v3`. Schema 2 replaced v1
(`/v1/feeds`, feed documents) in beacon v2.0.0.
