package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 22, 1, 0, 0, 0, time.UTC)

func TestComponentValidation(t *testing.T) {
	c := &Component{ID: "centric-fetch", UpdatedAt: t0, ExpectEvery: "10m",
		Gauges: []Metric{{Key: "records", Value: 1, Labels: Labels{"endpoint": "styles"}}, {Key: "records", Value: 2, Labels: Labels{"endpoint": "boms"}}},
		Checks: []Check{{Name: "healthy", Status: OK, Labels: Labels{"endpoint": "styles"}}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := &Component{ID: "Bad", Status: "great", ExpectEvery: "soon", Schedule: &Schedule{Grace: "x"}, Running: &Running{Trigger: "cron"},
		Gauges: []Metric{{Key: "a", Value: 1}, {Key: "a", Value: 2}}, Links: []Link{{Title: "x", URL: "ftp://x"}}}
	err := bad.Validate()
	for _, want := range []string{"id:", "updated_at", "expect_every", "schedule.grace", "running.started_at", "running.trigger", "status:", "appears twice", "links[0].url"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error doesn't mention %q: %v", want, err)
		}
	}
}

func TestRunAndEventValidation(t *testing.T) {
	code := 0
	r := &Run{Type: "run", Component: "sync", ID: "44b143fc085c4ecfa675a36c4d0696a0", StartedAt: t0, EndedAt: t0.Add(time.Minute), Outcome: Success, Trigger: "timer", ExitCode: &code}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	r.EndedAt, r.Outcome, r.Trigger = t0.Add(-time.Minute), "exploded", "cron"
	if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "before started_at") || !strings.Contains(err.Error(), "outcome") {
		t.Fatalf("bad run accepted: %v", err)
	}
	e := &Event{Type: "event", ID: "e1", At: t0, Kind: "deploy", Title: "api v2"}
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	e.Kind = "party"
	if e.Validate() == nil {
		t.Fatal("bad kind accepted")
	}
}

func TestComponentFiles(t *testing.T) {
	dir := t.TempDir()
	if err := WriteComponent(dir, &Component{ID: "host", UpdatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "other.json"), []byte(`{"id":"nope","updated_at":"2026-10-22T00:00:00Z"}`), 0o644)
	os.WriteFile(filepath.Join(dir, "typo.json"), []byte(`{"id":"typo","updated_at":"2026-10-22T00:00:00Z","stauts":"ok"}`), 0o644)
	os.Symlink(filepath.Join(dir, "host.json"), filepath.Join(dir, "link.json"))
	os.WriteFile(filepath.Join(dir, ".hidden.json"), []byte(`{}`), 0o644)
	comps, invalid, err := LoadComponents(dir)
	if err != nil || len(comps) != 1 || comps[0].ID != "host" {
		t.Fatalf("comps=%v err=%v", comps, err)
	}
	if len(invalid) != 2 {
		t.Fatalf("invalid = %+v, want other.json and typo.json", invalid)
	}
	if _, ok, _ := LoadComponent(dir, "../host"); ok {
		t.Fatal("path accepted")
	}
}
