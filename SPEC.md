# beacon v2: what a box tells its hub

A **box** runs beacon. beacon knows the box's **components** (the things you care about on
it: a nightly integration, a report job, a backup, the host itself) and keeps:

- **state**: what each component is now (gauges, checks, a schedule, a run in progress), a
  snapshot that's overwritten;
- **a log** of **runs** (every execution of a component's job) and **events** (reboots,
  deploys, config changes, notes), append-only.

A hub ([lookout](https://github.com/eemax/lookout)) reads both over beacon's read-only HTTP
API: the state on every poll, the log from where it left off, so it sees every run exactly
once. The design behind this is lookout's `docs/design.md`.

For people and agents building apps: [APPS.md](APPS.md) (what to expose, which numbers).

**Producers describe, hubs present.** Nothing here says how to draw anything; it says what
things are (units, labels, headline numbers, outcomes, schedules), and the hub derives the
display from that the same way for every component.

## Common shapes

**Metric** (a gauge of the state, or a stat of a run):

| Field | Required | Rules |
|---|---|---|
| `key` | yes | `[a-z0-9][a-z0-9._-]*`, at most 64 characters |
| `value` | yes | a finite number |
| `label` | no | at most 100; sentence case, without the component's name (`New records`). Hubs fall back to the key |
| `unit` | no | at most 16: `count`, `percent` (0-100), `bytes`, `s`, `ms`, or anything else (shown after the value) |
| `labels` | no | at most 4 `{"name": "value"}`: which of several things it's about (`{"endpoint": "styles"}`). Names like keys, values 1-64 characters without `, = { }` |
| `primary` | no | `true` for a headline number (at most 6 per list) |

A metric's identity is its key plus its labels (`records.new{endpoint=styles}`); each may
appear once per list.

**Check:** `name` (at most 100), `status` (`ok`, `warn`, `fail`, `unknown`), optional
`detail` (at most 500) and `labels`.

**Link:** `title` (at most 100) and `url` (`http(s)://`, at most 500).

**Times** are RFC 3339 strings; **durations** are like `90s`, `5m`, `26h`, `1h30m`.

Unknown fields are rejected by beacon, so a typo fails loudly; hubs ignore fields they
don't know. **Never put secrets in anything beacon serves.**

## Component (state)

```json
{
  "id": "centric-fetch",
  "title": "Centric fetch",
  "group": "Integrations",
  "description": "Pulls 12 Centric PLM endpoints into SQLite.",
  "updated_at": "2026-10-22T11:02:00Z",
  "expect_every": "10m",
  "schedule": { "next_at": "2026-10-23T01:00:00Z", "grace": "1h", "every": "Mon–Sat 01:00 UTC" },
  "running": { "id": "4f1c…", "started_at": "2026-10-22T01:00:02Z", "trigger": "timer" },
  "status": "ok",
  "summary": "12 endpoints, 364,972 records",
  "gauges": [ { "key": "records", "label": "Records", "value": 30538, "unit": "count", "labels": { "endpoint": "styles" } } ],
  "checks": [ { "name": "healthy", "status": "ok", "labels": { "endpoint": "styles" } } ],
  "links": [ { "title": "Files", "url": "https://example.com/files/" } ]
}
```

| Field | Required | Meaning |
|---|---|---|
| `id` | yes | `[a-z0-9][a-z0-9._-]*`, at most 64; stable |
| `title` | no | display name (at most 100) |
| `group` | no | a heading to group components under (at most 50), e.g. `Integrations`, `System` |
| `description` | no | one sentence: what it is (at most 300) |
| `updated_at` | yes | when this state was written |
| `expect_every` | no | longest normal gap between state updates; past it the state is **stale** |
| `schedule` | no | for components that run on a schedule: `next_at` (the next expected start), `grace` (how late a start may be before it counts as **missed**, default `1h`), `every` (human description) |
| `running` | no | a run in progress: `id`, `started_at`, `trigger` |
| `status`, `summary` | no | the producer's own verdict (`ok`/`warn`/`fail`/`unknown`) and one line (at most 500) about the state |
| `gauges` | no | at most 100 metrics: values now |
| `checks` | no | at most 50 |
| `links` | no | at most 10 |

## Log entries

Every entry has `seq` (a positive integer, increasing by one per entry on this box, never
reused) and `type`.

**Run** (written when the run ends):

```json
{
  "seq": 812, "type": "run", "component": "centric-fetch", "id": "4f1c…",
  "started_at": "2026-10-22T01:00:02Z", "ended_at": "2026-10-22T01:02:06Z",
  "outcome": "success", "trigger": "timer", "exit_code": 0, "unit": "centric-api-fetch.service",
  "summary": "delta fetch: 1,851 records, 697 new, 927 changed",
  "stats": [
    { "key": "records.new", "label": "New records", "value": 697, "unit": "count", "primary": true },
    { "key": "records.new", "label": "New", "value": 17, "unit": "count", "labels": { "endpoint": "styles" } }
  ],
  "error": null,
  "artifacts": [ { "title": "Report", "url": "https://example.com/report.xlsx" } ]
}
```

| Field | Required | Meaning |
|---|---|---|
| `component` | yes | the component's `id` |
| `id` | yes | unique per box (systemd's InvocationID, or a random id), at most 64 |
| `started_at`, `ended_at` | yes | |
| `outcome` | yes | `success`, `failure` or `cancelled` (stopped from outside) |
| `trigger` | no | `timer`, `manual`, `boot`, `dependency`, `other` |
| `exit_code` | no | the process's exit status |
| `unit` | no | the systemd unit or command that ran |
| `summary` | no | one line (at most 500) |
| `stats` | no | at most 100 metrics: what the run produced |
| `error` | no | why it failed, at most 2000 characters (the end of its output if nothing better) |
| `artifacts` | no | at most 10 links: what it made |

**Event:**

```json
{ "seq": 813, "type": "event", "id": "b7…", "at": "2026-10-22T00:00:41Z", "kind": "reboot",
  "component": null, "title": "Rebooted", "detail": "kernel 6.8.0-146 → 6.8.0-147" }
```

| Field | Required | Meaning |
|---|---|---|
| `id` | yes | unique per box, at most 64 |
| `at` | yes | |
| `kind` | yes | `reboot`, `deploy`, `config`, `note`, `service` (a long-running service crashed, or failed) |
| `component` | no | the component it's about, or null for the whole box |
| `title` | yes | at most 200 |
| `detail` | no | at most 2000 |

The box keeps the log for 90 days (at most 20,000 entries).

## HTTP API

`beacon serve` listens on `127.0.0.1:7600` (in tunnel mode on the box's private tunnel
address). Every request except `/healthz` needs `Authorization: Bearer <token>`; only
`GET`/`HEAD` are answered.

**`GET /v2/state`** (with `ETag`; `If-None-Match` → `304`):

```json
{
  "schema": 2,
  "source": "ysera",
  "beacon": "v2.0.0",
  "generated_at": "2026-10-22T11:02:10Z",
  "box": { "os": "Ubuntu 24.04.5 LTS", "kernel": "6.8.0-147-generic", "arch": "amd64",
           "boot_at": "2026-10-22T00:00:30Z" },
  "log": { "first_seq": 1, "last_seq": 813 },
  "components": [ { "id": "host", "...": "..." } ],
  "invalid": [ { "file": "broken.json", "error": "…" } ]
}
```

**`GET /v2/log?after=<seq>&limit=<n>`**: entries with `seq > after`, oldest first, at most
`limit` (default and maximum 500):

```json
{ "entries": [ { "seq": 812, "type": "run", "...": "..." } ], "first_seq": 1, "last_seq": 813 }
```

A hub keeps the last `seq` it stored and asks for more while `last_seq` is ahead. If its
`after` is older than `first_seq - 1`, entries were pruned before it saw them (a gap it
can note).

`GET /healthz` → `ok`. Anything else is `404`.

## How a hub presents it (what producers can rely on)

- **Status per component**, worst of: latest run failed (until a later run succeeds) ·
  **missed** (`schedule.next_at + grace` passed with no run started) · a check failing or
  warning · **stale** state · the producer's `status`. Always shown with its reason.
- **Headline numbers**: `primary` metrics (stats of the latest run first, then gauges), as
  tiles; with none marked, the first unlabelled ones.
- **Tables from labels**: metrics with labels group by label *names* into a table: a row per
  label value, a column per key (header = its `label`); checks with the same labels become
  that table's status column.
- **History**: run stats chart one point per run, compared with the median of earlier runs
  ("3× usual"); gauges chart over time.
- **Units** decide formatting everywhere; `links` and `artifacts` become buttons.
- **Group** headings order the components; within a box, problems first.

## Versioning

`schema` changes only when a reader would break. New optional fields may appear within
schema 2. v1 (`/v1/feeds`, feed documents) was replaced by this in beacon v2.0.0.
