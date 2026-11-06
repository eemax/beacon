package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eemax/beacon/internal/logbook"
	"github.com/eemax/beacon/internal/spec"
	"github.com/eemax/beacon/internal/tokens"
)

func setup(t *testing.T) (*httptest.Server, string, string) {
	t.Helper()
	dir := t.TempDir()
	comps := filepath.Join(dir, "components")
	os.Mkdir(comps, 0o755)
	tok, err := tokens.Add(filepath.Join(dir, "tokens"), "lookout")
	if err != nil {
		t.Fatal(err)
	}
	spec.WriteComponent(comps, &spec.Component{ID: "host", UpdatedAt: time.Now().UTC().Truncate(time.Second)})
	os.WriteFile(filepath.Join(comps, "broken.json"), []byte("{"), 0o644)
	l := logbook.Log{Path: filepath.Join(dir, "log.jsonl")}
	for i := 0; i < 3; i++ {
		l.Append(&spec.Event{Type: "event", ID: fmt.Sprintf("e%d", i), At: time.Now().UTC(), Kind: "note", Title: "n"})
	}
	srv := &Server{Components: comps, Definitions: filepath.Join(dir, "components.d"), LogPath: l.Path, Source: "test", Version: "test",
		Tokens: tokens.Open(filepath.Join(dir, "tokens")), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: time.Now}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, tok, dir
}

func get(t *testing.T, url, token string, header ...string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestState(t *testing.T) {
	ts, tok, _ := setup(t)
	if r := get(t, ts.URL+"/v2/state", ""); r.StatusCode != 401 {
		t.Fatalf("no token: %d", r.StatusCode)
	}
	r := get(t, ts.URL+"/v2/state", tok)
	var body state
	json.NewDecoder(r.Body).Decode(&body)
	if body.Schema != 2 || len(body.Components) != 1 || len(body.Invalid) != 1 || body.Log.First != 1 || body.Log.Last != 3 || body.Box.Arch == "" {
		t.Fatalf("state = %+v", body)
	}
	etag := r.Header.Get("ETag")
	if r := get(t, ts.URL+"/v2/state", tok, "If-None-Match", etag); r.StatusCode != 304 {
		t.Fatalf("unchanged: %d", r.StatusCode)
	}
}

func TestLog(t *testing.T) {
	ts, tok, _ := setup(t)
	r := get(t, ts.URL+"/v2/log?after=1&limit=1", tok)
	var body struct {
		Entries []map[string]any `json:"entries"`
		First   int64            `json:"first_seq"`
		Last    int64            `json:"last_seq"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if len(body.Entries) != 1 || body.Entries[0]["seq"].(float64) != 2 || body.Last != 3 {
		t.Fatalf("log = %+v", body)
	}
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		req, _ := http.NewRequest(method, ts.URL+"/v2/log", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		if resp.StatusCode != 405 {
			t.Errorf("%s: %d", method, resp.StatusCode)
		}
	}
	for _, path := range []string{"/v1/feeds", "/v2/state/../tokens", "/"} {
		if r := get(t, ts.URL+path, tok); r.StatusCode != 404 {
			t.Errorf("%s: %d", path, r.StatusCode)
		}
	}
	if r := get(t, ts.URL+"/healthz", ""); r.StatusCode != 200 {
		t.Fatal("healthz")
	}
}

func TestLogServesActorAndLinks(t *testing.T) {
	ts, tok, dir := setup(t)
	e := &spec.Event{Type: "event", ID: "e9", At: time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC), Kind: "change", Title: "rotated the key",
		Actor: &spec.Actor{Kind: "agent", Name: "acmd"},
		Links: []spec.EventLink{{Title: "commit", Ref: &spec.Ref{Kind: "commit", ID: "abc123"}}, {Title: "Diff", URL: "https://example.com/d"}}}
	if _, err := (logbook.Log{Path: filepath.Join(dir, "log.jsonl")}).Append(e); err != nil {
		t.Fatal(err)
	}
	r := get(t, ts.URL+"/v2/log?after=3", tok)
	var body struct {
		Entries []json.RawMessage `json:"entries"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	want := `{"seq":4,"type":"event","id":"e9","at":"2026-11-06T09:00:00Z","kind":"change","title":"rotated the key",` +
		`"actor":{"kind":"agent","name":"acmd"},"links":[{"title":"commit","ref":{"kind":"commit","id":"abc123"}},{"title":"Diff","url":"https://example.com/d"}]}`
	if len(body.Entries) != 1 || string(body.Entries[0]) != want {
		t.Fatalf("entries:\n got %s\nwant %s", body.Entries, want)
	}
}
