package brief

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/eemax/beacon/internal/spec"
)

var now = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

func fresh(id string) *spec.Component {
	return &spec.Component{ID: id, UpdatedAt: now.Add(-30 * time.Second)}
}

func TestDerive(t *testing.T) {
	failed := &spec.Run{Component: "x", StartedAt: now.Add(-2 * time.Hour), EndedAt: now.Add(-time.Hour), Outcome: spec.Failure, Error: "a\nconnection refused\n"}
	cases := []struct {
		name   string
		c      func() *spec.Component
		latest *spec.Run
		state  State
		reason string
	}{
		{"quiet", func() *spec.Component { return fresh("x") }, nil, OK, ""},
		{"producer summary", func() *spec.Component { c := fresh("x"); c.Summary = "16 files"; return c }, nil, OK, "16 files"},
		{"last run ok", func() *spec.Component { return fresh("x") }, &spec.Run{Outcome: spec.Success, Summary: "fetched 12"}, OK, "fetched 12"},
		{"unknown", func() *spec.Component { c := fresh("x"); c.Status = spec.Unknown; return c }, nil, Unknown, "the box can't tell"},
		{"stale default 10m", func() *spec.Component { c := fresh("x"); c.UpdatedAt = now.Add(-13 * time.Minute); return c }, nil, Stale, "no update since 2026-10-24T11:47:00Z"},
		{"stale own expect_every", func() *spec.Component {
			c := fresh("x")
			c.UpdatedAt, c.ExpectEvery = now.Add(-13*time.Minute), "1h"
			return c
		}, nil, OK, ""},
		{"failed run: error's last line", func() *spec.Component { return fresh("x") }, failed, Fail, "last run (2026-10-24T10:00:00Z) failed: connection refused"},
		{"failed run: summary first", func() *spec.Component { return fresh("x") }, &spec.Run{StartedAt: now, Outcome: spec.Failure, Summary: "1 of 12 failed", Error: "x"}, Fail, "last run (2026-10-24T12:00:00Z) failed: 1 of 12 failed"},
		{"cancelled", func() *spec.Component { return fresh("x") }, &spec.Run{StartedAt: now, Outcome: spec.Cancelled}, Warn, "last run (2026-10-24T12:00:00Z) was stopped"},
		{"missed", func() *spec.Component {
			c := fresh("x")
			c.Schedule = &spec.Schedule{NextAt: ptr(now.Add(-3 * time.Hour)), Grace: "1h"}
			return c
		}, &spec.Run{StartedAt: now.Add(-27 * time.Hour), Outcome: spec.Success}, Missed, "missed the run due 2026-10-24T09:00:00Z"},
		{"not missed: within grace", func() *spec.Component {
			c := fresh("x")
			c.Schedule = &spec.Schedule{NextAt: ptr(now.Add(-30 * time.Minute))}
			return c
		}, nil, OK, ""},
		{"not missed: running", func() *spec.Component {
			c := fresh("x")
			c.Schedule = &spec.Schedule{NextAt: ptr(now.Add(-3 * time.Hour))}
			c.Running = &spec.Running{StartedAt: now.Add(-3 * time.Hour)}
			return c
		}, nil, OK, ""},
		{"not missed: it ran", func() *spec.Component {
			c := fresh("x")
			c.Schedule = &spec.Schedule{NextAt: ptr(now.Add(-3 * time.Hour))}
			return c
		}, &spec.Run{StartedAt: now.Add(-3*time.Hour + time.Minute), Outcome: spec.Success}, OK, "last run succeeded"},
		{"checks: the failing one, then a count", func() *spec.Component {
			c := fresh("x")
			c.Checks = []spec.Check{{Name: "disk", Status: spec.Warn, Detail: "86 %"}, {Name: "reachable", Status: spec.Fail, Detail: "HTTP 502", Labels: spec.Labels{"probe": "site"}}, {Name: "ok", Status: spec.OK}}
			return c
		}, nil, Fail, "reachable · site: HTTP 502 (+1 more)"},
		{"producer status", func() *spec.Component { c := fresh("x"); c.Status, c.Summary = spec.Warn, "credits low"; return c }, nil, Warn, "credits low"},
		{"worst wins: fail over stale", func() *spec.Component { c := fresh("x"); c.UpdatedAt = now.Add(-time.Hour); return c }, failed, Fail, "last run (2026-10-24T10:00:00Z) failed: connection refused"},
	}
	for _, tc := range cases {
		got := Derive(tc.c(), tc.latest, now)
		if got.State != tc.state || got.Reason != tc.reason {
			t.Errorf("%s: got %s %q, want %s %q", tc.name, got.State, got.Reason, tc.state, tc.reason)
		}
	}
}

func TestFormat(t *testing.T) {
	cases := []struct {
		v    float64
		unit string
		want string
	}{
		{367138, "count", "367,138"},
		{-1234567, "", "-1,234,567"},
		{0.5, "", "0.5"},
		{0.004, "", "0.004"},
		{18.4321, "percent", "18.43 %"},
		{1.1 * (1 << 30), "bytes", "1.1 GB"},
		{900, "bytes", "900 B"},
		{45, "s", "45s"},
		{192, "s", "3m12s"},
		{7500, "s", "2h05m"},
		{3*86400 + 4*3600, "s", "3d4h"},
		{47.7, "ms", "47.7 ms"},
		{58.5, "usd", "$58.50"},
		{12, "eur", "12.00 EUR"},
		{3, "files", "3 files"},
	}
	for _, tc := range cases {
		if got := Format(tc.v, tc.unit); got != tc.want {
			t.Errorf("Format(%v, %q) = %q, want %q", tc.v, tc.unit, got, tc.want)
		}
	}
}

func raw(t *testing.T, entries ...any) []json.RawMessage {
	var out []json.RawMessage
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

func TestWrite(t *testing.T) {
	host := fresh("host")
	host.Title = "Host"
	host.Gauges = []spec.Metric{{Key: "load", Label: "Load", Value: 0.21}, {Key: "disk", Label: "Disk used", Value: 45.3, Unit: "percent", Primary: true}}
	fetch := fresh("fetch")
	fetch.Title = "Nightly fetch"
	fetch.Schedule = &spec.Schedule{Every: "daily at 01:00", NextAt: ptr(now.Add(13 * time.Hour))}
	fetch.Checks = []spec.Check{{Name: "a", Status: spec.Fail, Detail: "x"}, {Name: "b", Status: spec.Warn}}
	backup := fresh("backup")
	backup.Running = &spec.Running{StartedAt: now.Add(-5 * time.Minute), Trigger: "timer"}

	run := func(c string, ago time.Duration, o spec.Outcome, summary string) spec.Run {
		return spec.Run{Type: "run", Component: c, ID: c + ago.String(), StartedAt: now.Add(-ago - time.Minute), EndedAt: now.Add(-ago), Outcome: o, Trigger: "timer", ExitCode: ptr(0), Summary: summary}
	}
	failedRun := run("fetch", 11*time.Hour, spec.Failure, "")
	failedRun.ExitCode, failedRun.Error = ptr(1), "Traceback\nTimeoutError: read timed out\n"
	log := raw(t,
		run("fetch", 30*time.Hour, spec.Success, "too old"),
		run("backup", 20*time.Hour, spec.Success, "run one"),
		run("backup", 10*time.Hour, spec.Success, "second"),
		run("backup", 2*time.Hour, spec.Success, "latest backup"),
		failedRun,
		spec.Event{Type: "event", ID: "e1", At: now.Add(-3 * time.Hour), Kind: "deploy", Component: "fetch", Title: "v1.2", Detail: "fix\nretry backoff"},
	)
	var b strings.Builder
	if err := Write(&b, Input{Source: "ysera", Components: []*spec.Component{backup, fetch, host}, Invalid: []spec.Invalid{{File: "bad.json", Error: "not valid"}}, Log: log, Now: now, Window: 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	want := []string{
		"# ysera: 2 components need attention\n",
		"beacon brief at 2026-10-24T12:00:00Z.",
		// Problems first: fetch (fail) before backup and host.
		"## Components\n\n- fail **fetch** (Nightly fetch): last run (2026-10-24T00:59:00Z) failed: TimeoutError: read timed out\n  - fail a: x\n  - warn b\n  - schedule: daily at 01:00; next run 2026-10-25T01:00:00Z\n",
		"- ok **backup**: latest backup\n  - running since 2026-10-24T11:55:00Z (timer)\n",
		"- ok **host** (Host)\n  - Disk used 45.3 %\n",
		"- fail **bad.json**: not a valid component: not valid\n",
		"## Recent (since 2026-10-23T12:00:00Z, newest first)\n\n",
		"- 2026-10-24T10:00:00Z run **backup** success after 1m00s (timer): latest backup (+2 earlier successful runs)\n",
		"- 2026-10-24T09:00:00Z deploy **fetch**: v1.2: fix retry backoff\n",
		"- 2026-10-24T01:00:00Z run **fetch** failure after 1m00s (timer), exit 1: TimeoutError: read timed out\n",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
	if strings.Contains(out, "too old") || strings.Contains(out, "run one") {
		t.Errorf("runs outside the window or superseded successes shown:\n%s", out)
	}
	if strings.Index(out, "**fetch** (Nightly") > strings.Index(out, "**backup**: latest") {
		t.Errorf("problems should come first:\n%s", out)
	}
}

func TestWriteEmptyAndCapped(t *testing.T) {
	var b strings.Builder
	Write(&b, Input{Source: "box", Now: now, Window: time.Hour})
	if out := b.String(); !strings.Contains(out, "# box: no components yet") || !strings.Contains(out, "Nothing logged.") {
		t.Errorf("empty brief:\n%s", out)
	}
	var events []any
	for i := range maxRecent + 5 {
		events = append(events, spec.Event{Type: "event", ID: fmt.Sprintf("e%d", i), At: now.Add(-time.Duration(i) * time.Minute), Kind: "note", Title: fmt.Sprintf("note %d", i)})
	}
	b.Reset()
	Write(&b, Input{Source: "box", Components: []*spec.Component{fresh("x")}, Log: raw(t, events...), Now: now, Window: 24 * time.Hour})
	out := b.String()
	if !strings.Contains(out, "# box: all components ok") || !strings.Contains(out, "- … 5 more (`beacon log --limit 200`)") || strings.Contains(out, "note 44") {
		t.Errorf("capped brief:\n%s", out)
	}
}
