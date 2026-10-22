# Beacon feed format, schema 1

A **source** (a box running beacon) publishes **feeds**: small JSON documents, one file
per feed, in its feeds folder (`/var/lib/beacon/feeds/<feed>.json`). A hub
([lookout](https://github.com/eemax/lookout)) reads them over beacon's read-only HTTP API,
keeps the history, draws the dashboard and sends the alerts.

A feed says how one thing on the box is doing: the box itself, a nightly integration,
a backup, a report run. Anything that can write a file can publish one, usually through
`beacon put` at the end of a job, or a collector script that `beacon collect` runs on a
timer.

**Producers describe, hubs present.** A feed never says how to draw anything. It says
what each number *is* (unit, which thing it's about, whether it's a headline), and the hub
derives the layout from that, the same way for every feed. Adding a feed or a metric never
needs a change in the hub.

## Document

```json
{
  "schema": 1,
  "feed": "nightly-sync",
  "title": "Nightly sync",
  "status": "ok",
  "summary": "delta sync, 1,851 records in 2.1 min: 697 new, 927 changed",
  "updated_at": "2026-10-22T01:02:12Z",
  "expect_every": "26h",
  "metrics": [
    { "key": "duration", "label": "Duration", "value": 124, "unit": "s", "primary": true },
    { "key": "records.new", "label": "New", "value": 697, "unit": "count", "primary": true },
    { "key": "records.total", "label": "Records", "value": 30538, "unit": "count", "labels": { "endpoint": "styles" } },
    { "key": "records.new", "label": "New", "value": 17, "unit": "count", "labels": { "endpoint": "styles" } },
    { "key": "records.total", "label": "Records", "value": 16274, "unit": "count", "labels": { "endpoint": "boms" } },
    { "key": "records.new", "label": "New", "value": 29, "unit": "count", "labels": { "endpoint": "boms" } }
  ],
  "checks": [
    { "name": "healthy", "status": "ok", "labels": { "endpoint": "styles" } },
    { "name": "healthy", "status": "warn", "detail": "2 pages retried", "labels": { "endpoint": "boms" } }
  ],
  "links": [{ "title": "Run log", "url": "https://example.com/runs/latest" }]
}
```

| Field | Required | Rules |
|---|---|---|
| `schema` | yes | `1` |
| `feed` | yes | Name, same as the file name without `.json`. `[a-z0-9][a-z0-9._-]*`, at most 64 characters |
| `title` | no | Display name, at most 100 characters. Hubs fall back to `feed` |
| `status` | yes | `ok`, `warn`, `fail` or `unknown`: the producer's own verdict |
| `summary` | no | One line for people, at most 500 characters |
| `updated_at` | yes | RFC 3339 time the producer last wrote the feed (`beacon put` sets it) |
| `expect_every` | no | Longest normal gap between updates, as a duration (`90s`, `5m`, `26h`). After `updated_at + expect_every` the hub treats the feed as **stale**, whatever its `status` says: a producer that died can't report it. Leave it out for feeds that only change on events |
| `metrics` | no | At most 100 (below) |
| `checks` | no | At most 50 (below) |
| `links` | no | At most 10. Each: `title` (at most 100) and `url` (`http(s)://`, at most 500): where to look further (a report folder, a run log) |

**Metric:** `key` (same pattern as `feed`), `value` (a finite number), and optionally
`label` (at most 100), `unit` (at most 16), `labels`, `primary`. A metric is identified by
its key plus its labels: `records.new` and `records.new{endpoint=styles}` are different
series, and each may appear once.

**Check:** `name` (at most 100), `status` (as above), optionally `detail` (at most 500)
and `labels`.

**Labels:** at most 4 per metric or check, `{"name": "value"}`, names like `feed`, values
1-64 characters without `, = { }`. They say which thing a number or check is about.

Unknown fields are rejected, so a typo fails loudly instead of disappearing. A document
is at most 256 KiB. **Never put secrets in a feed**: feeds are status, not storage.

## How hubs present feeds

This is the contract that lets a producer get a good display without designing one.
lookout follows it; another hub should too.

- **Headline numbers.** Metrics with `"primary": true` (at most 6) are the feed's headline:
  shown as stat tiles with a trend line, on the overview and on the feed's page, in the
  order given. With none marked, the first unlabelled metrics stand in.
- **Other unlabelled metrics** are shown as further stat tiles on the feed's page.
- **Tables from labels.** Metrics with labels are grouped by their label *names*. Each
  group is a table: one row per distinct label value combination (in order of first
  appearance), one column per metric key (header = the metric's `label`, else the key).
  Checks with the same label names join that table as a status column, matched by label
  values. So `records.total{endpoint=…}`, `records.new{endpoint=…}` and a check
  `healthy{endpoint=…}` become one "by endpoint" table with a status per row. Keep a
  key's `label` the same across rows: it's the column header.
- **Checks without labels** are listed on their own, problems first.
- **Units format the number:** `count` (thousands separators, compact in tiles), `percent`
  (0-100), `bytes`, `s`/`ms` (durations); anything else is shown after the value.
  No unit: a plain number.
- **History:** every metric (each key+labels series) gets a trend line from the values the
  hub saw. A metric whose meaning changes should get a new key.
- **Status:** the feed's own `status`, except **stale** (above), which the hub derives.

Write titles and labels in sentence case without the feed's name in them (`New`, not
`Centric: new records`): the hub shows them in the feed's context.

## Status semantics

- `ok`: working as intended.
- `warn`: working, but someone should look (disk filling, a slow run, one endpoint failing).
- `fail`: not working (job failed, service down).
- `unknown`: the producer can't tell (e.g. a dependency it asks is unavailable).

## Reading feeds: beacon's HTTP API

`beacon serve` listens on `127.0.0.1:7600` by default (in tunnel mode on the box's private
tunnel address) and serves only the feeds folder, read-only. Every request except
`/healthz` needs `Authorization: Bearer <token>` (tokens: `beacon token add <name>`).

| Request | Response |
|---|---|
| `GET /v1/feeds` | All feeds in one document (below). Carries an `ETag`; send `If-None-Match` to get `304` when nothing changed |
| `GET /v1/feeds/<feed>` | One feed document |
| `GET /healthz` | `ok` (no token needed; says nothing about feeds) |

Anything else is `404`, any method but `GET`/`HEAD` is `405`.

```json
{
  "schema": 1,
  "source": "web-1",
  "beacon": "v0.3.0",
  "generated_at": "2026-10-22T01:02:00Z",
  "feeds": [ { "schema": 1, "feed": "host", "...": "..." } ],
  "invalid": [ { "file": "broken.json", "error": "status: must be ok, warn, fail or unknown" } ]
}
```

`invalid` lists files in the folder that aren't valid feeds, so a broken producer shows
up on the hub instead of silently vanishing. Only regular `*.json` files directly in the
folder are read: no subfolders, no symlinks, nothing else on the box.

## Versioning

`schema` changes only for changes that would break a reader. New optional fields can be
added within schema 1; hubs ignore fields they don't know when reading (beacon itself
rejects them when writing, so producers stay on the documented set). Removed in v0.3.0:
`visibility` (lookout has no public page; everything is behind its login).
