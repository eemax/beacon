# Beacon feed format, schema 1

A **source** (a box running beacon) publishes **feeds**: small JSON documents, one file
per feed, in its feeds folder (`/var/lib/beacon/feeds/<feed>.json`). A hub
([lookout](https://github.com/eemax/lookout)) reads them over beacon's read-only HTTP API
and keeps the history, draws the dashboard and sends the alerts.

A feed says how one thing on the box is doing: the box itself, a nightly integration,
a backup, a report run. Anything that can write a file can publish one, usually through
`beacon put` at the end of a job, or a collector script that `beacon collect` runs on a
timer.

## Document

```json
{
  "schema": 1,
  "feed": "nightly-sync",
  "title": "Nightly sync",
  "status": "ok",
  "summary": "12 endpoints fetched, 0 failing",
  "updated_at": "2026-10-22T01:01:12Z",
  "expect_every": "26h",
  "visibility": "private",
  "metrics": [
    { "key": "records.new", "label": "New records", "value": 12, "unit": "count" },
    { "key": "duration", "value": 58, "unit": "s", "visibility": "public" }
  ],
  "checks": [
    { "name": "endpoints fresh", "status": "ok" },
    { "name": "lock", "status": "warn", "detail": "held by a run from 02:00" }
  ]
}
```

| Field | Required | Rules |
|---|---|---|
| `schema` | yes | `1` |
| `feed` | yes | Name, same as the file name without `.json`. `[a-z0-9][a-z0-9._-]*`, at most 64 characters |
| `title` | no | Display name, at most 100 characters. The hub falls back to `feed` |
| `status` | yes | `ok`, `warn`, `fail` or `unknown`: the producer's own verdict |
| `summary` | no | One line for people, at most 500 characters |
| `updated_at` | yes | RFC 3339 time the producer last wrote the feed (`beacon put` sets it) |
| `expect_every` | no | Longest normal gap between updates, as a duration (`90s`, `5m`, `26h`). After `updated_at + expect_every` the hub treats the feed as **stale**, whatever its `status` says: a producer that died can't report it. Leave it out for feeds that only change on events |
| `visibility` | no | `private` (default) or `public`. Public feeds may appear on the hub's public page |
| `metrics` | no | At most 100. Each: `key` (same pattern as `feed`, at most 64), `value` (a finite number), optional `label` (at most 100), `unit` (at most 16, e.g. `count`, `s`, `bytes`, `percent`) and `visibility` (overrides the feed's) |
| `checks` | no | At most 50. Each: `name` (at most 100), `status` (as above), optional `detail` (at most 500) |

Unknown fields are rejected, so a typo fails loudly instead of disappearing. A document
is at most 256 KiB.

Metrics are numbers on purpose: the hub keeps their history and charts them. Text goes
in `summary` or a check's `detail`. A metric key should mean the same thing on every
update (rename = new series).

**Never put secrets in a feed.** Private feeds are only shown to people signed in to the
hub, but they're status, not storage.

## Status semantics

- `ok`: working as intended.
- `warn`: working, but someone should look (disk filling, a slow run, one endpoint failing).
- `fail`: not working (job failed, service down).
- `unknown`: the producer can't tell (e.g. a dependency it asks is unavailable).

The hub derives one more state itself: **stale**, as above.

## Reading feeds: beacon's HTTP API

`beacon serve` listens on `127.0.0.1:7600` by default and serves only the feeds folder,
read-only. Every request except `/healthz` needs `Authorization: Bearer <token>` (tokens:
`beacon token add <name>`).

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
  "beacon": "0.1.0",
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
rejects them when writing, so producers stay on the documented set).
