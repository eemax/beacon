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
