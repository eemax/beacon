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
	if c.Status != spec.Fail || !strings.Contains(c.Summary, "oops") {
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
	if err != nil || c.Status != spec.Fail || !strings.Contains(c.Summary, "appears twice") {
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
	c := &spec.Component{ID: "x", UpdatedAt: time.Now()}
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
