package spec

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
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

func TestEventActorAndLinks(t *testing.T) {
	ok := &Event{Type: "event", ID: "e1", At: t0, Kind: "change", Title: "rotated the API key",
		Actor: &Actor{Kind: "agent", Name: "acmd"},
		Links: []EventLink{
			{Title: "Commit", Ref: &Ref{Kind: "commit", ID: "abc123"}},
			{Title: "Diff", URL: "https://example.com/compare/a...b"},
		}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"person", "agent", "unit"} {
		e := *ok
		e.Actor = &Actor{Kind: k, Name: strings.Repeat("n", 100)}
		if err := e.Validate(); err != nil {
			t.Errorf("actor kind %s: %v", k, err)
		}
	}
	for _, k := range []string{"commit", "alert", "incident", "conversation", "other"} {
		e := *ok
		e.Links = []EventLink{{Title: strings.Repeat("t", 100), Ref: &Ref{Kind: k, ID: strings.Repeat("i", 200)}}}
		if err := e.Validate(); err != nil {
			t.Errorf("ref kind %s: %v", k, err)
		}
	}
	at10 := *ok
	at10.Links = nil
	for i := 0; i < 10; i++ {
		at10.Links = append(at10.Links, EventLink{Title: "x", URL: "http://example.com/" + strings.Repeat("u", 481)}) // 500 characters
	}
	if err := at10.Validate(); err != nil {
		t.Errorf("10 links of 500 characters: %v", err)
	}

	cases := map[string]func(e *Event){
		"actor.kind":                  func(e *Event) { e.Actor.Kind = "robot" },
		"actor.name: required":        func(e *Event) { e.Actor.Name = " " },
		"actor.name: 101 characters":  func(e *Event) { e.Actor.Name = strings.Repeat("n", 101) },
		"links: 11, at most 10":       func(e *Event) { e.Links = append(at10.Links, EventLink{Title: "x", URL: "https://x"}) },
		"links[0].title: required":    func(e *Event) { e.Links[0].Title = "" },
		"links[0].title: 101":         func(e *Event) { e.Links[0].Title = strings.Repeat("t", 101) },
		"links[0]: has both":          func(e *Event) { e.Links[0].URL = "https://x" },
		"links[0]: needs a url":       func(e *Event) { e.Links[0].Ref = nil },
		"links[0].ref.kind":           func(e *Event) { e.Links[0].Ref = &Ref{Kind: "pr", ID: "1"} },
		"links[0].ref.id: required":   func(e *Event) { e.Links[0].Ref = &Ref{Kind: "commit"} },
		"links[0].ref.id: 201":        func(e *Event) { e.Links[0].Ref = &Ref{Kind: "commit", ID: strings.Repeat("i", 201)} },
		"links[1].url":                func(e *Event) { e.Links[1].URL = "ftp://example.com" },
		"links[1].url: must be":       func(e *Event) { e.Links[1].URL = "https://example.com/" + strings.Repeat("u", 481) },
		"links[1].url: must be an ht": func(e *Event) { e.Links[1].URL = "https://example.com/a b" },
	}
	for want, mutate := range cases {
		e := *ok
		a := *ok.Actor
		e.Actor = &a
		e.Links = append([]EventLink(nil), ok.Links...)
		r := *ok.Links[0].Ref
		e.Links[0].Ref = &r
		mutate(&e)
		if err := e.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
}

func TestOldEventsStillValid(t *testing.T) {
	// An entry as beacon wrote it before actor and links existed.
	line := `{"seq":813,"type":"event","id":"b7","at":"2026-10-22T00:00:41Z","kind":"reboot","title":"Rebooted","detail":"kernel 6.8.0-146 → 6.8.0-147"}`
	var e Event
	if err := Decode([]byte(line), &e); err != nil {
		t.Fatal(err)
	}
	if err := e.Validate(); err != nil || e.Actor != nil || e.Links != nil {
		t.Fatalf("old event: %+v, %v", e, err)
	}
	// And a new one round-trips with the fields as SPEC names them.
	line = `{"seq":814,"type":"event","id":"c9","at":"2026-10-22T09:14:03Z","kind":"change","title":"t","actor":{"kind":"unit","name":"cert-renew.service"},"links":[{"title":"Commit","ref":{"kind":"commit","id":"4e1a2b7"}},{"title":"Changes","url":"https://example.com/c"}]}`
	e = Event{}
	if err := Decode([]byte(line), &e); err != nil {
		t.Fatal(err)
	}
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(&e)
	if string(out) != line {
		t.Fatalf("round trip:\n got %s\nwant %s", out, line)
	}
	if err := Decode([]byte(`{"type":"event","actor":{"kind":"agent","name":"a","email":"x"}}`), &Event{}); err == nil {
		t.Fatal("unknown actor field accepted")
	}
}

func TestURLNeedsAHost(t *testing.T) {
	for _, u := range []string{"http://", "https://", "https:///path", "https:///p", "http://:8080/x", "https://?q=1", "https://?q", "https://#f", "https://user@/x", "https://user@:80/x"} {
		c := &Component{ID: "x", UpdatedAt: t0, Links: []Link{{Title: "t", URL: u}}}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "links[0].url") {
			t.Errorf("component link %q: %v", u, err)
		}
		r := &Run{Type: "run", Component: "x", ID: "r1", StartedAt: t0, EndedAt: t0, Outcome: Success, Artifacts: []Link{{Title: "t", URL: u}}}
		if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "artifacts[0].url") {
			t.Errorf("run artifact %q: %v", u, err)
		}
		e := &Event{Type: "event", ID: "e1", At: t0, Kind: "note", Title: "t", Links: []EventLink{{Title: "t", URL: u}}}
		if err := e.Validate(); err == nil || !strings.Contains(err.Error(), "links[0].url") {
			t.Errorf("event link %q: %v", u, err)
		}
	}
	// Everything beacon v2.5.0 accepted that has a host stays valid, url.Parse or not.
	for _, u := range []string{"http://localhost", "https://example.com", "https://example.com:8443/a?b=c#d", "http://10.249.0.1:7600/",
		"https://e.com/%zz", "https://e.com/a\rb", "https://e.com:abc/", "https://ex|ample.com/", "http://[::1]:7600/", "https://é.com/",
		"https://user:pw@e.com/", "https://@e.com/", "https://e.com?q", "https://e.com#f"} {
		c := &Component{ID: "x", UpdatedAt: t0, Links: []Link{{Title: "t", URL: u}}}
		e := &Event{Type: "event", ID: "e1", At: t0, Kind: "note", Title: "t", Links: []EventLink{{Title: "t", URL: u}}}
		if err := errors.Join(c.Validate(), e.Validate()); err != nil {
			t.Errorf("%q refused: %v", u, err)
		}
	}
	// 500 code points is the limit, not 500 bytes.
	u := "https://example.com/" + strings.Repeat("é", 480)
	c := &Component{ID: "x", UpdatedAt: t0, Links: []Link{{Title: "t", URL: u}}}
	if err := c.Validate(); err != nil {
		t.Errorf("500 code points refused: %v", err)
	}
	c.Links[0].URL += "é"
	if c.Validate() == nil {
		t.Error("501 code points accepted")
	}
}

// SPEC: an event kind is a token [a-z][a-z0-9-]{0,31}; beacon's own kinds are such tokens.
func TestKindsAreTokens(t *testing.T) {
	token := regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	for k := range Kinds {
		if !token.MatchString(string(k)) {
			t.Errorf("kind %q isn't a token", k)
		}
	}
}
