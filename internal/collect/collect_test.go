package collect

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eemax/beacon/internal/logbook"
	"github.com/eemax/beacon/internal/spec"
	"github.com/eemax/beacon/internal/statefile"
	"github.com/eemax/beacon/internal/systemd"
)

func config(t *testing.T) Config {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "components.d"), 0o755)
	return Config{Components: filepath.Join(dir, "components"), Definitions: filepath.Join(dir, "components.d"), Log: filepath.Join(dir, "log.jsonl"),
		State: filepath.Join(dir, "state.json"), Timeout: 2 * time.Second, Now: time.Now}
}

func script(t *testing.T, dir, name, body string) string {
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunFromUnitOutcomes(t *testing.T) {
	d := &Definition{ID: "fetch"}
	start := time.Date(2026, 10, 22, 1, 0, 4, 0, time.UTC)
	timers := []systemd.Timer{{Unit: "fetch.timer", Activates: "fetch.service", Last: start.Add(-2 * time.Second)}}
	cases := []struct {
		result       string
		code, status int
		want         spec.Outcome
		exit         int
	}{
		{"success", 1, 0, spec.Success, 0},
		{"exit-code", 1, 1, spec.Failure, 1},
		{"exit-code", 1, 143, spec.Cancelled, 143}, // stopped, exited cleanly on SIGTERM
		{"signal", 2, 15, spec.Cancelled, 143},
		{"signal", 2, 9, spec.Failure, 137},
		{"timeout", 2, 15, spec.Failure, 143}, // hung: killed by TimeoutStartSec
	}
	for _, c := range cases {
		u := systemd.Unit{Name: "fetch.service", InvocationID: "abc", Result: c.result, Start: start, Exit: start.Add(2 * time.Minute), ExitCode: c.code, ExitStatus: c.status}
		r := runFromUnit(d, u, timers)
		if r.Outcome != c.want || *r.ExitCode != c.exit || r.Trigger != "timer" || r.EndedAt.Sub(r.StartedAt) != 2*time.Minute {
			t.Errorf("%s/%d/%d: %s exit %d trigger %s", c.result, c.code, c.status, r.Outcome, *r.ExitCode, r.Trigger)
		}
	}
	d.Systemd = &struct {
		Units   []string `json:"units"`
		Timers  []string `json:"timers,omitempty"`
		Trigger string   `json:"trigger,omitempty"`
	}{Units: []string{"reports.service"}, Trigger: "dependency"}
	if r := runFromUnit(d, systemd.Unit{Name: "reports.service", InvocationID: "x", Result: "success", Start: start}, timers); r.Trigger != "dependency" {
		t.Errorf("no timer: trigger %s, want the definition's", r.Trigger)
	}
}

func TestDefinitionsAndAdapters(t *testing.T) {
	cfg := config(t)
	state := script(t, cfg.Definitions, "photos.state", `echo '{"summary":"12 albums","gauges":[{"key":"albums","value":12,"primary":true}],"checks":[{"name":"disk","status":"ok"}]}'`)
	runAdapter := script(t, cfg.Definitions, "photos.run", `echo "{\"summary\":\"$BEACON_RUN_OUTCOME: 5 new\",\"stats\":[{\"key\":\"photos.new\",\"value\":5,\"primary\":true}]}"`)
	def := map[string]any{"id": "photos", "title": "Photos sync", "group": "Media", "state": state, "run": runAdapter, "schedule": map[string]any{"interval": "6h", "every": "every 6 h"}}
	data, _ := json.Marshal(def)
	os.WriteFile(filepath.Join(cfg.Definitions, "photos.json"), data, 0o644)
	os.WriteFile(filepath.Join(cfg.Definitions, "broken.json"), []byte(`{"id":"other"}`), 0o644)
	os.WriteFile(filepath.Join(cfg.Definitions, "typo.json"), []byte(`{"id":"typo","staet":"x"}`), 0o644)

	defs, invalid := Definitions(cfg.Definitions)
	if len(defs) != 1 || len(invalid) != 2 {
		t.Fatalf("defs=%d invalid=%+v", len(defs), invalid)
	}
	// A run through FinishRun (what exec does) gets the run adapter's stats.
	start := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	code := 0
	run := &spec.Run{Type: "run", Component: "photos", ID: "r1", StartedAt: start, EndedAt: start.Add(time.Minute), Outcome: spec.Success, ExitCode: &code}
	if err := FinishRun(context.Background(), cfg, defs["photos"], run); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), cfg)
	if err != nil || res.Failed != 0 {
		t.Fatalf("collect: %v %+v", err, res)
	}
	c, ok, err := spec.LoadComponent(cfg.Components, "photos")
	if !ok || err != nil {
		t.Fatalf("component: %v", err)
	}
	if c.Title != "Photos sync" || c.Summary != "12 albums" || len(c.Gauges) != 1 || c.Schedule == nil || c.Schedule.NextAt == nil || !c.Schedule.NextAt.Equal(start.Add(6*time.Hour)) {
		t.Fatalf("component = %+v (schedule %+v)", c, c.Schedule)
	}
	entries, _, _, _ := logbook.Log{Path: cfg.Log}.Read(0, 10)
	if len(entries) != 1 || !strings.Contains(string(entries[0]), `"summary":"success: 5 new"`) {
		t.Fatalf("log = %s", entries)
	}
	// A definition that's gone takes its component with it.
	os.Remove(filepath.Join(cfg.Definitions, "photos.json"))
	Run(context.Background(), cfg)
	if _, ok, _ := spec.LoadComponent(cfg.Components, "photos"); ok {
		t.Fatal("component of a removed definition kept")
	}
}

func TestBrokenStateAdapterAndVanishedExec(t *testing.T) {
	cfg := config(t)
	bad := script(t, cfg.Definitions, "x.state", `echo 'oops' >&2; exit 4`)
	data, _ := json.Marshal(map[string]any{"id": "x", "state": bad})
	os.WriteFile(filepath.Join(cfg.Definitions, "x.json"), data, 0o644)
	// A `beacon exec` whose process is gone (pid 0x7ffffffe won't exist).
	statefile.Update(cfg.State, func(s *statefile.State) error {
		s.Running["gone"] = statefile.Marker{Component: "x", StartedAt: time.Now().Add(-time.Hour).UTC(), PID: 0x7ffffffe}
		return nil
	})
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	c, _, _ := spec.LoadComponent(cfg.Components, "x")
	if c.Status != spec.Unknown || c.Reporting == nil || !strings.Contains(c.Reporting.Error, "oops") {
		t.Fatalf("component = %+v", c)
	}
	entries, _, _, _ := logbook.Log{Path: cfg.Log}.Read(0, 10)
	if len(entries) != 1 || !strings.Contains(string(entries[0]), `"outcome":"failure"`) {
		t.Fatalf("vanished exec not logged: %s", entries)
	}
	st, _ := statefile.Read(cfg.State)
	if len(st.Running) != 0 {
		t.Fatal("marker left behind")
	}
}

func TestExecOnlyComponent(t *testing.T) {
	cfg := config(t)
	now := time.Now().UTC().Truncate(time.Second)
	code := 0
	run := &spec.Run{Type: "run", Component: "backup", ID: NewID(), StartedAt: now.Add(-time.Minute), EndedAt: now, Outcome: spec.Success, Trigger: "manual", ExitCode: &code}
	if err := FinishRun(context.Background(), cfg, nil, run); err != nil {
		t.Fatal(err)
	}
	statefile.Update(cfg.State, func(s *statefile.State) error {
		s.LastStart["old"] = now.AddDate(0, 0, -100)
		s.LastStart["host"] = now
		return nil
	})
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if c, _, err := spec.LoadComponent(cfg.Components, "backup"); err != nil || c.ExpectEvery != "10m" {
		t.Fatalf("backup = %+v, %v", c, err)
	}
	if _, ok, _ := spec.LoadComponent(cfg.Components, "old"); ok {
		t.Fatal("a component that last ran 100 days ago came back")
	}
	if _, ok, _ := spec.LoadComponent(cfg.Components, "host"); ok {
		t.Fatal("host written from an exec run")
	}
	st, _ := statefile.Read(cfg.State)
	if _, ok := st.LastStart["old"]; ok {
		t.Fatal("old LastStart not pruned")
	}
}

func TestBadAdapterOutputNeverCostsTheRun(t *testing.T) {
	cfg := config(t)
	// Valid JSON, invalid stats (bad key, NaN-free but a duplicate): the run must still be
	// logged, without the adapter's part, and the component must say why.
	badRun := script(t, cfg.Definitions, "x.run", `echo '{"summary":"ok","stats":[{"key":"Bad Key","value":1}]}'`)
	badState := script(t, cfg.Definitions, "x.state", `echo '{"status":"ok","gauges":[{"key":"a","value":1},{"key":"a","value":2}]}'`)
	data, _ := json.Marshal(map[string]any{"id": "x", "run": badRun, "state": badState})
	os.WriteFile(filepath.Join(cfg.Definitions, "x.json"), data, 0o644)
	defs, _ := Definitions(cfg.Definitions)
	now := time.Now().UTC().Truncate(time.Second)
	code := 0
	run := &spec.Run{Type: "run", Component: "x", ID: "r1", StartedAt: now.Add(-time.Minute), EndedAt: now, Outcome: spec.Success, ExitCode: &code}
	if err := FinishRun(context.Background(), cfg, defs["x"], run); err != nil {
		t.Fatalf("run lost: %v", err)
	}
	entries, _, _, _ := logbook.Log{Path: cfg.Log}.Read(0, 10)
	if len(entries) != 1 || strings.Contains(string(entries[0]), "Bad Key") {
		t.Fatalf("log = %s", entries)
	}
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	c, _, err := spec.LoadComponent(cfg.Components, "x")
	if err != nil || c.Status == spec.Fail || c.Reporting == nil || !strings.Contains(c.Reporting.Error, "appears twice") {
		t.Fatalf("component = %+v, %v", c, err)
	}
	last := c.Checks[len(c.Checks)-1]
	if last.Name != "beacon" || !strings.Contains(last.Detail, "without its stats") {
		t.Fatalf("checks = %+v", c.Checks)
	}
	// The adapter works again: the warning goes.
	os.WriteFile(badRun, []byte("#!/bin/sh\necho '{}'\n"), 0o755)
	run.ID = "r2"
	FinishRun(context.Background(), cfg, defs["x"], run)
	Run(context.Background(), cfg)
	c, _, _ = spec.LoadComponent(cfg.Components, "x")
	for _, k := range c.Checks {
		if k.Name == "beacon" {
			t.Fatalf("warning kept: %+v", k)
		}
	}
}

func TestDefinitionsRejectWhatCantBeWritten(t *testing.T) {
	cfg := config(t)
	os.WriteFile(filepath.Join(cfg.Definitions, "host.json"), []byte(`{"id":"host"}`), 0o644)
	os.WriteFile(filepath.Join(cfg.Definitions, "long.json"), []byte(`{"id":"long","title":"`+strings.Repeat("x", 101)+`"}`), 0o644)
	os.WriteFile(filepath.Join(cfg.Definitions, "grace.json"), []byte(`{"id":"grace","schedule":{"grace":"soon"}}`), 0o644)
	defs, invalid := Definitions(cfg.Definitions)
	if len(defs) != 0 || len(invalid) != 3 {
		t.Fatalf("defs=%v invalid=%+v", defs, invalid)
	}
}

func TestServices(t *testing.T) {
	cfg := config(t)
	d := &Definition{ID: "files"}
	now := time.Now().UTC().Truncate(time.Second)
	caddy := systemd.Unit{Name: "caddy.service", Type: "notify", InvocationID: "a1", ActiveState: "active", ActiveSince: now.Add(-time.Hour), Memory: 64 << 20}
	dufs := systemd.Unit{Name: "dufs.service", Type: "simple", InvocationID: "b1", ActiveState: "active", ActiveSince: now.Add(-time.Hour), Memory: -1}
	c := &spec.Component{ID: "files", UpdatedAt: now}
	res := &Result{}
	if s := serviceState(cfg, d, []systemd.Unit{caddy, dufs}, now, c, res); s != "caddy, dufs are up" {
		t.Fatalf("summary %q", s)
	}
	if len(c.Checks) != 2 || len(c.Gauges) != 5 { // uptime×2, restarts×2, memory×1
		t.Fatalf("checks %+v gauges %+v", c.Checks, c.Gauges)
	}
	// dufs crashed and systemd restarted it; caddy was restarted on purpose (no event).
	dufs.InvocationID, dufs.Restarts = "b2", 1
	caddy.InvocationID = "a2"
	serviceState(cfg, d, []systemd.Unit{caddy, dufs}, now, &spec.Component{ID: "files", UpdatedAt: now}, res)
	// Then caddy fails for good.
	caddy.ActiveState, caddy.Result = "failed", "exit-code"
	c = &spec.Component{ID: "files", UpdatedAt: now}
	if s := serviceState(cfg, d, []systemd.Unit{caddy, dufs}, now, c, res); s != "caddy failed" {
		t.Fatalf("summary %q", s)
	}
	if c.Checks[0].Status != spec.Fail {
		t.Fatalf("checks %+v", c.Checks)
	}
	serviceState(cfg, d, []systemd.Unit{caddy, dufs}, now, &spec.Component{ID: "files", UpdatedAt: now}, res) // still failed: no second event
	entries, _, _, _ := logbook.Log{Path: cfg.Log}.Read(0, 10)
	if len(entries) != 2 || !strings.Contains(string(entries[0]), "dufs.service crashed") || !strings.Contains(string(entries[1]), "caddy.service failed") {
		t.Fatalf("events: %s", entries)
	}
}

func TestFit(t *testing.T) {
	c := &spec.Component{ID: "x", Class: spec.Service, UpdatedAt: time.Now()}
	for i := 0; i < 8; i++ {
		c.Gauges = append(c.Gauges, spec.Metric{Key: "a", Value: 1, Primary: true, Labels: spec.Labels{"n": strings.Repeat("x", i+1)}})
	}
	c.Gauges = append(c.Gauges, spec.Metric{Key: "a", Value: 2, Labels: spec.Labels{"n": "x"}}) // a duplicate
	fit(c)
	if err := c.Validate(); err != nil || len(c.Gauges) != 8 || c.Gauges[0].Value != 1 {
		t.Fatalf("%v %+v", err, c.Gauges)
	}
}

func TestStateEvery(t *testing.T) {
	cfg := config(t)
	counter := filepath.Join(t.TempDir(), "n")
	adapter := script(t, cfg.Definitions, "slow.state", `echo x >> `+counter+`; echo '{"status":"ok","summary":"fine"}'`)
	data, _ := json.Marshal(map[string]any{"id": "slow", "state": adapter, "state_every": "5m"})
	os.WriteFile(filepath.Join(cfg.Definitions, "slow.json"), data, 0o644)
	start := time.Now()
	for i, at := range []time.Duration{0, time.Minute, 4 * time.Minute, 6 * time.Minute} {
		cfg.Now = func() time.Time { return start.Add(at) }
		if _, err := Run(context.Background(), cfg); err != nil {
			t.Fatal(i, err)
		}
	}
	runs, _ := os.ReadFile(counter)
	if n := strings.Count(string(runs), "x"); n != 2 { // at 0 and at 6 min
		t.Fatalf("adapter ran %d times", n)
	}
	c, _, _ := spec.LoadComponent(cfg.Components, "slow")
	if c.Summary != "fine" {
		t.Fatalf("cached result not used: %+v", c)
	}
	os.WriteFile(filepath.Join(cfg.Definitions, "bad.json"), []byte(`{"id":"bad","state_every":"5m"}`), 0o644)
	if _, invalid := Definitions(cfg.Definitions); len(invalid) != 1 {
		t.Fatal("state_every without a state adapter accepted")
	}
}

func TestTimerNext(t *testing.T) {
	next := time.Date(2026, 11, 3, 10, 10, 0, 0, time.UTC)
	timers := []systemd.Timer{
		{Unit: "notes-mirror.timer", Activates: "notes-mirror.service"}, // its run in progress: no next elapse
		{Unit: "backup.timer", Activates: "backup.service", Next: next},
		{Unit: "stopped.timer", Activates: "stopped.service"},
	}
	running := func(u string) bool { return u == "notes-mirror.service" }
	if n, ok := timerNext("notes-mirror.timer", timers, running); !ok || !n.IsZero() {
		t.Errorf("running: %v %v", n, ok)
	}
	if n, ok := timerNext("backup.timer", timers, running); !ok || !n.Equal(next) {
		t.Errorf("scheduled: %v %v", n, ok)
	}
	if _, ok := timerNext("stopped.timer", timers, running); ok {
		t.Error("a stopped timer whose unit isn't running counts as scheduled")
	}
	if _, ok := timerNext("missing.timer", timers, running); ok {
		t.Error("a missing timer counts as scheduled")
	}
}

// define writes a definition with a state adapter whose script body can change.
func define(t *testing.T, cfg Config, id, body string, extra map[string]any) string {
	t.Helper()
	adapter := script(t, cfg.Definitions, id+".state", body)
	def := map[string]any{"id": id, "state": adapter}
	for k, v := range extra {
		def[k] = v
	}
	data, _ := json.Marshal(def)
	if err := os.WriteFile(filepath.Join(cfg.Definitions, id+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return adapter
}

func collectAt(t *testing.T, cfg Config, at time.Time) {
	t.Helper()
	cfg.Now = func() time.Time { return at }
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
}

func load(t *testing.T, cfg Config, id string) *spec.Component {
	t.Helper()
	c, ok, err := spec.LoadComponent(cfg.Components, id)
	if !ok || err != nil {
		t.Fatalf("component %s: %v %v", id, ok, err)
	}
	return c
}

const goodOut = `echo '{"status":"warn","summary":"fine","gauges":[{"key":"n","value":3}],"items":[{"id":"a","title":"A"}]}'`

func TestReportingFirstRunFailsThenGoodAgain(t *testing.T) {
	cfg := config(t)
	start := time.Now().UTC().Truncate(time.Second)
	adapter := define(t, cfg, "x", `echo boom >&2; exit 1`, nil)
	collectAt(t, cfg, start)
	c := load(t, cfg, "x")
	if c.Status != spec.Unknown || c.Reporting == nil || c.Reporting.LastGoodAt != nil || !c.Reporting.Since.Equal(start) || !strings.Contains(c.Reporting.Error, "boom") {
		t.Fatalf("no good output yet: %+v / %+v", c, c.Reporting)
	}
	if c.Class != spec.Service {
		t.Fatalf("class = %q", c.Class)
	}

	// Good: clears reporting, and its output is what's kept.
	os.WriteFile(adapter, []byte("#!/bin/sh\n"+goodOut+"\n"), 0o755)
	collectAt(t, cfg, start.Add(time.Minute))
	c = load(t, cfg, "x")
	if c.Reporting != nil || c.Status != spec.Warn || len(c.Items) != 1 || c.Gauges[0].Key != "n" {
		t.Fatalf("good again: %+v", c)
	}

	// Bad again, with the last good state kept: reporting set, the status is the kept one,
	// never fail on this account; since is when this failure began, and it holds.
	os.WriteFile(adapter, []byte("#!/bin/sh\nexit 3\n"), 0o755)
	collectAt(t, cfg, start.Add(2*time.Minute))
	collectAt(t, cfg, start.Add(3*time.Minute))
	c = load(t, cfg, "x")
	r := c.Reporting
	if r == nil || c.Status != spec.Warn || c.Summary != "fine" || len(c.Items) != 1 || c.Gauges[0].Value != 3 {
		t.Fatalf("last good not kept: %+v / %+v", c, r)
	}
	if r.LastGoodAt == nil || !r.LastGoodAt.Equal(start.Add(time.Minute)) || !r.Since.Equal(start.Add(2*time.Minute)) {
		t.Fatalf("reporting times: %+v", r)
	}

	// The first good output clears it and replaces the kept state.
	os.WriteFile(adapter, []byte("#!/bin/sh\necho '{\"status\":\"ok\",\"summary\":\"new\"}'\n"), 0o755)
	collectAt(t, cfg, start.Add(4*time.Minute))
	c = load(t, cfg, "x")
	if c.Reporting != nil || c.Summary != "new" || len(c.Items) != 0 {
		t.Fatalf("not cleared: %+v", c)
	}
	os.WriteFile(adapter, []byte("#!/bin/sh\nexit 1\n"), 0o755)
	collectAt(t, cfg, start.Add(5*time.Minute))
	c = load(t, cfg, "x")
	if c.Summary != "new" || c.Reporting == nil || !c.Reporting.Since.Equal(start.Add(5*time.Minute)) {
		t.Fatalf("kept state wasn't replaced: %+v / %+v", c, c.Reporting)
	}
}

func TestReportingInvalidDocumentIsNotWrittenAsIs(t *testing.T) {
	for name, doc := range map[string]string{
		"unknown field": `{"status":"ok","stauts":"ok"}`,
		"bad class":     `{"status":"ok","class":"critical"}`,
		"class":         `{"status":"ok","class":"service"}`,
		"reporting":     `{"status":"ok","reporting":{"error":"x","since":"2026-11-01T00:00:00Z","last_good_at":null}}`,
		"bad status":    `{"status":"degraded"}`,
		"not json":      `not json`,
		"bad item":      `{"items":[{"id":"a","title":"A","waiting":false}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := config(t)
			start := time.Now().UTC().Truncate(time.Second)
			adapter := define(t, cfg, "x", goodOut, nil)
			collectAt(t, cfg, start)
			os.WriteFile(adapter, []byte("#!/bin/sh\ncat <<'EOF'\n"+doc+"\nEOF\n"), 0o755)
			collectAt(t, cfg, start.Add(time.Minute))
			c := load(t, cfg, "x")
			if c.Reporting == nil || c.Status == spec.Fail || c.Summary != "fine" || c.Class != spec.Service {
				t.Fatalf("%+v / %+v", c, c.Reporting)
			}
		})
	}
}

func TestReportingHungAdapterIsKilledAndTheNextComponentGoesOn(t *testing.T) {
	cfg := config(t)
	cfg.Timeout = 2 * time.Second
	start := time.Now().UTC().Truncate(time.Second)
	hang := define(t, cfg, "a-hang", goodOut, nil)
	define(t, cfg, "b-next", `echo '{"status":"ok","summary":"next"}'`, map[string]any{"class": "advisory"})
	collectAt(t, cfg, start)
	os.WriteFile(hang, []byte("#!/bin/sh\nsleep 60\n"), 0o755)
	began := time.Now()
	collectAt(t, cfg, start.Add(time.Minute))
	if took := time.Since(began); took > 20*time.Second {
		t.Fatalf("collect waited %s on a hung adapter", took)
	}
	c := load(t, cfg, "a-hang")
	if c.Reporting == nil || !strings.Contains(c.Reporting.Error, "timed out") || c.Summary != "fine" || c.Status != spec.Warn {
		t.Fatalf("hung: %+v / %+v", c, c.Reporting)
	}
	if n := load(t, cfg, "b-next"); n.Summary != "next" || n.Class != spec.Advisory || n.UpdatedAt.Before(start.Add(time.Minute)) {
		t.Fatalf("next component: %+v", n)
	}
}

func TestReportingStateEvery(t *testing.T) {
	cfg := config(t)
	counter := filepath.Join(t.TempDir(), "n")
	start := time.Now().UTC().Truncate(time.Second)
	adapter := define(t, cfg, "slow", "echo x >> "+counter+"\n"+goodOut, map[string]any{"state_every": "5m"})
	collectAt(t, cfg, start) // good, cached
	os.WriteFile(adapter, []byte("#!/bin/sh\necho x >> "+counter+"\nexit 1\n"), 0o755)
	collectAt(t, cfg, start.Add(6*time.Minute)) // runs, fails, the failure is cached
	failing := load(t, cfg, "slow")
	if failing.Reporting == nil || failing.Summary != "fine" || !failing.Reporting.Since.Equal(start.Add(6*time.Minute)) {
		t.Fatalf("failure: %+v / %+v", failing, failing.Reporting)
	}
	collectAt(t, cfg, start.Add(8*time.Minute)) // the cached failure: no run, still reporting, same since
	c := load(t, cfg, "slow")
	if runs, _ := os.ReadFile(counter); strings.Count(string(runs), "x") != 2 {
		t.Fatalf("adapter ran %d times, want 2", strings.Count(string(runs), "x"))
	}
	if c.Reporting == nil || c.Summary != "fine" || c.Status != spec.Warn || !c.Reporting.Since.Equal(failing.Reporting.Since) {
		t.Fatalf("cached failure: %+v / %+v", c, c.Reporting)
	}
	// A cached good output (from a run elsewhere) after a failure clears reporting.
	good, _ := json.Marshal(cached{At: start.Add(9 * time.Minute), Out: json.RawMessage(`{"status":"ok","summary":"cached good"}`)})
	os.WriteFile(filepath.Join(filepath.Dir(cfg.State), "cache", "slow.json"), good, 0o600)
	collectAt(t, cfg, start.Add(10*time.Minute))
	c = load(t, cfg, "slow")
	if c.Reporting != nil || c.Summary != "cached good" {
		t.Fatalf("cached good: %+v / %+v", c, c.Reporting)
	}
}

func TestReportingCheckDefinitions(t *testing.T) {
	cfg := config(t)
	data := t.TempDir()
	okState := script(t, cfg.Definitions, "ok.state", `echo '{"status":"ok"}'`)
	okRun := script(t, cfg.Definitions, "ok.run", `echo '{"summary":"1 record","stats":[{"key":"n","value":1}]}'`)
	write := func(id string, def map[string]any) {
		def["id"] = id
		b, _ := json.Marshal(def)
		os.WriteFile(filepath.Join(cfg.Definitions, id+".json"), b, 0o644)
	}
	write("ok", map[string]any{"state": okState, "run": okRun})
	write("plain", map[string]any{})
	write("exit", map[string]any{"state": script(t, cfg.Definitions, "exit.state", `exit 2`)})
	write("json", map[string]any{"state": script(t, cfg.Definitions, "json.state", `echo nope`)})
	write("class", map[string]any{"state": script(t, cfg.Definitions, "class.state", `echo '{"class":"list"}'`)})
	write("hang", map[string]any{"state": script(t, cfg.Definitions, "hang.state", `sleep 60`)})
	write("badrun", map[string]any{"run": script(t, cfg.Definitions, "badrun.run", `echo '{"stats":[{"key":"Bad Key","value":1}]}'`)})
	write("env", map[string]any{"state": script(t, cfg.Definitions, "env.state", `[ "$BEACON_COMPONENT" = env ] && echo '{}'`)})
	os.WriteFile(filepath.Join(cfg.Definitions, "broken.json"), []byte(`{"id":"broken","state_every":"5m"}`), 0o644)

	lines, bad := CheckDefinitions(context.Background(), cfg.Definitions, nil, 2*time.Second)
	got := map[string]string{}
	for _, l := range lines {
		status, rest, _ := strings.Cut(l, " ")
		id, why, _ := strings.Cut(rest, ":")
		got[id] = status + why
	}
	for _, id := range []string{"ok", "plain", "env"} {
		if got[id] != "ok" {
			t.Errorf("%s: %q", id, got[id])
		}
	}
	for id, want := range map[string]string{"exit": "state adapter", "json": "state adapter", "class": "state adapter", "hang": "timed out",
		"badrun": "run adapter", "broken": "definition"} {
		if !strings.HasPrefix(got[id], "BAD") || !strings.Contains(got[id], want) {
			t.Errorf("%s: %q", id, got[id])
		}
	}
	if bad != 6 {
		t.Errorf("bad = %d: %v", bad, lines)
	}
	// Only the listed ones; an id without a definition is bad; all valid: none bad.
	if lines, bad = CheckDefinitions(context.Background(), cfg.Definitions, []string{"ok", "plain"}, time.Second); bad != 0 || len(lines) != 2 {
		t.Errorf("only valid: %v (%d)", lines, bad)
	}
	if lines, bad = CheckDefinitions(context.Background(), cfg.Definitions, []string{"ok", "missing"}, time.Second); bad != 1 || lines[len(lines)-1] != "BAD missing: no definition" {
		t.Errorf("unknown id: %v (%d)", lines, bad)
	}
	if entries, _ := os.ReadDir(data); len(entries) != 0 {
		t.Errorf("wrote %v", entries)
	}
}
