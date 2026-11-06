package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/eemax/beacon/internal/logbook"
	"github.com/eemax/beacon/internal/statefile"

	"github.com/eemax/beacon/internal/spec"
)

func TestParseMetric(t *testing.T) {
	cases := map[string]spec.Metric{
		"n=3":                              {Key: "n", Value: 3},
		"duration=58:s":                    {Key: "duration", Value: 58, Unit: "s"},
		"records.new=12:count:New records": {Key: "records.new", Value: 12, Unit: "count", Label: "New records"},
		"records.new{endpoint=styles}=17:count:New": {Key: "records.new", Value: 17, Unit: "count", Label: "New", Labels: spec.Labels{"endpoint": "styles"}},
		"x{a=1,b=2}=0.5": {Key: "x", Value: 0.5, Labels: spec.Labels{"a": "1", "b": "2"}},
	}
	for in, want := range cases {
		got, err := parseMetric(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"n", "n=x", "n{a=1=3", "n{a}=1"} {
		if _, err := parseMetric(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseCheck(t *testing.T) {
	got, err := parseCheck("fresh{endpoint=styles}=warn:2 h old: retrying")
	want := spec.Check{Name: "fresh", Status: "warn", Detail: "2 h old: retrying", Labels: spec.Labels{"endpoint": "styles"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestExecLogsRuns(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BEACON_DATA", dir)
	t.Setenv("BEACON_ETC", dir)
	if err := cmdExec([]string{"photos", "--trigger", "timer", "--", "sh", "-c", "echo synced 12"}); err != nil {
		t.Fatalf("success run: %v", err)
	}
	err := cmdExec([]string{"photos", "--", "sh", "-c", "echo working; echo 'disk full' >&2; exit 3"})
	if code, ok := err.(exitError); !ok || code != 3 {
		t.Fatalf("failing run: %v, want exit 3", err)
	}
	if err := cmdExec([]string{"photos", "--", "/nonexistent/cmd"}); err == nil {
		t.Fatal("missing command succeeded")
	}
	entries, _, last, err := logbook.Log{Path: filepath.Join(dir, "log.jsonl")}.Read(0, 10)
	if err != nil || last != 3 {
		t.Fatalf("log: %d entries, last %d, %v", len(entries), last, err)
	}
	var runs []spec.Run
	for _, e := range entries {
		var r spec.Run
		json.Unmarshal(e, &r)
		runs = append(runs, r)
	}
	if runs[0].Outcome != spec.Success || runs[0].Trigger != "timer" || *runs[0].ExitCode != 0 {
		t.Errorf("run 1: %+v", runs[0])
	}
	if runs[1].Outcome != spec.Failure || *runs[1].ExitCode != 3 || !strings.Contains(runs[1].Error, "disk full") {
		t.Errorf("run 2: %+v", runs[1])
	}
	if runs[2].Outcome != spec.Failure || *runs[2].ExitCode != 127 {
		t.Errorf("run 3: %+v", runs[2])
	}
	st, _ := statefile.Read(filepath.Join(dir, "state.json"))
	if len(st.Running) != 0 {
		t.Errorf("running markers left: %v", st.Running)
	}
}

func TestEventAndRunCommands(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BEACON_DATA", dir)
	if err := cmdEvent([]string{"deploy", "--title", "api v2", "--component", "api"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdEvent([]string{"reboot", "--title", "x"}); err == nil {
		t.Fatal("reboot events are collect's")
	}
	doc := filepath.Join(dir, "run.json")
	os.WriteFile(doc, []byte(`{"started_at":"2026-10-22T01:00:00Z","ended_at":"2026-10-22T01:02:00Z","outcome":"success","stats":[{"key":"n","value":3}]}`), 0o644)
	if err := cmdRun([]string{"sync", "--json", doc}); err != nil {
		t.Fatal(err)
	}
	entries, _, _, _ := logbook.Log{Path: filepath.Join(dir, "log.jsonl")}.Read(0, 10)
	if len(entries) != 2 || !strings.Contains(string(entries[1]), `"component":"sync"`) {
		t.Fatalf("entries: %s", entries)
	}
}

func TestEventActorAndLinks(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BEACON_DATA", dir)
	t.Setenv("BEACON_ETC", dir)
	logPath := filepath.Join(dir, "log.jsonl")
	if err := cmdEvent([]string{"change", "--title", "rotated the API key", "--actor-kind", "agent", "--actor", "acmd",
		"--ref", "commit=commit:abc123", "--link", "Diff=https://example.com/c?a=1", "--ref", "chat=conversation:main:42"}); err != nil {
		t.Fatal(err)
	}
	entries, _, _, _ := logbook.Log{Path: logPath}.Read(0, 10)
	if len(entries) != 1 {
		t.Fatalf("entries: %s", entries)
	}
	var e spec.Event
	if err := spec.Decode(entries[0], &e); err != nil {
		t.Fatal(err)
	}
	want := []spec.EventLink{
		{Title: "commit", Ref: &spec.Ref{Kind: "commit", ID: "abc123"}},
		{Title: "Diff", URL: "https://example.com/c?a=1"},
		{Title: "chat", Ref: &spec.Ref{Kind: "conversation", ID: "main:42"}},
	}
	if e.Kind != "change" || !reflect.DeepEqual(e.Actor, &spec.Actor{Kind: "agent", Name: "acmd"}) || !reflect.DeepEqual(e.Links, want) {
		t.Fatalf("event: %+v", e)
	}
	// Without the new flags an event carries neither field.
	if err := cmdEvent([]string{"note", "--title", "plain"}); err != nil {
		t.Fatal(err)
	}
	entries, _, _, _ = logbook.Log{Path: logPath}.Read(1, 10)
	if s := string(entries[0]); strings.Contains(s, "actor") || strings.Contains(s, "links") {
		t.Fatalf("plain event: %s", s)
	}

	tooMany := []string{"deploy", "--title", "x"}
	for i := 0; i < 11; i++ {
		tooMany = append(tooMany, "--link", "l=https://example.com/")
	}
	refused := map[string][]string{
		"go together":          {"deploy", "--title", "x", "--actor", "acmd"},
		"go together ":         {"deploy", "--title", "x", "--actor-kind", "agent"},
		"actor.kind":           {"deploy", "--title", "x", "--actor-kind", "robot", "--actor", "r2"},
		"actor.name: 101":      {"deploy", "--title", "x", "--actor-kind", "person", "--actor", strings.Repeat("n", 101)},
		"links: 11, at most":   tooMany,
		"want title=url":       {"deploy", "--title", "x", "--link", "https://example.com"},
		"want title=kind:id":   {"deploy", "--title", "x", "--ref", "commit=abc123"},
		"ref.kind":             {"deploy", "--title", "x", "--ref", "PR=pr:12"},
		"ref.id: 201":          {"deploy", "--title", "x", "--ref", "c=commit:" + strings.Repeat("i", 201)},
		"links[0].title: 101":  {"deploy", "--title", "x", "--ref", strings.Repeat("t", 101) + "=commit:abc"},
		"links[0].url":         {"deploy", "--title", "x", "--link", "l=ftp://example.com"},
		"links[0].title: requ": {"deploy", "--title", "x", "--link", "=https://example.com"},
		"usage":                {"deploy", "--title", "x", "stray"},
	}
	for want, args := range refused {
		if err := cmdEvent(args); err == nil || !strings.Contains(err.Error(), strings.TrimSpace(want)) {
			t.Errorf("%v: got %v, want %q", args, err, want)
		}
	}
	if _, _, last, _ := (logbook.Log{Path: logPath}).Read(0, 100); last != 2 {
		t.Fatalf("refused events were logged: last seq %d", last)
	}
}

func TestValidateOldAndNewEvents(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BEACON_DATA", dir)
	t.Setenv("BEACON_ETC", dir)
	logPath := filepath.Join(dir, "log.jsonl")
	old := `{"seq":1,"type":"event","id":"a1","at":"2026-10-22T00:00:41Z","kind":"deploy","title":"v1"}` + "\n"
	os.WriteFile(logPath, []byte(old), 0o644)
	os.WriteFile(logPath+".seq", []byte("2\n"), 0o644)
	if err := cmdEvent([]string{"change", "--title", "t", "--actor-kind", "unit", "--actor", "renew.service", "--ref", "a=alert:9"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdValidate(nil); err != nil {
		t.Fatalf("validate: %v", err)
	}
	bad := `{"seq":3,"type":"event","id":"a3","at":"2026-10-22T00:00:41Z","kind":"change","title":"t","actor":{"kind":"robot","name":"x"}}` + "\n"
	f, _ := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(bad)
	f.Close()
	if err := cmdValidate(nil); err == nil {
		t.Fatal("validate passed an event with a bad actor")
	}
}
