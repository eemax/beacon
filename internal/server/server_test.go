package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eemax/beacon/internal/feed"
	"github.com/eemax/beacon/internal/tokens"
)

func setup(t *testing.T) (*httptest.Server, string, string) {
	t.Helper()
	dir := t.TempDir()
	feeds := filepath.Join(dir, "feeds")
	os.Mkdir(feeds, 0o755)
	tokensPath := filepath.Join(dir, "tokens")
	token, err := tokens.Add(tokensPath, "lookout")
	if err != nil {
		t.Fatal(err)
	}
	f := &feed.Feed{Schema: 1, Feed: "host", Status: feed.OK, UpdatedAt: time.Unix(1792600000, 0).UTC()}
	if err := feed.Write(feeds, f); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(feeds, "broken.json"), []byte("{"), 0o644)
	os.WriteFile(filepath.Join(dir, "secret.json"), []byte(`{"schema":1}`), 0o644)
	srv := &Server{
		Dir: feeds, Source: "test", Version: "test", Tokens: tokens.Open(tokensPath),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: time.Now,
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, token, feeds
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

func TestListingNeedsAToken(t *testing.T) {
	ts, token, _ := setup(t)
	if resp := get(t, ts.URL+"/v1/feeds", ""); resp.StatusCode != 401 {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	if resp := get(t, ts.URL+"/v1/feeds", "bcn_wrong"); resp.StatusCode != 401 {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	resp := get(t, ts.URL+"/v1/feeds", token)
	if resp.StatusCode != 200 {
		t.Fatalf("good token: %d", resp.StatusCode)
	}
	var body listing
	json.NewDecoder(resp.Body).Decode(&body)
	if body.Source != "test" || len(body.Feeds) != 1 || body.Feeds[0].Feed != "host" {
		t.Fatalf("listing = %+v", body)
	}
	if len(body.Invalid) != 1 || body.Invalid[0].File != "broken.json" {
		t.Fatalf("invalid = %+v", body.Invalid)
	}
}

func TestETagGives304UntilAFeedChanges(t *testing.T) {
	ts, token, feeds := setup(t)
	etag := get(t, ts.URL+"/v1/feeds", token).Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	if resp := get(t, ts.URL+"/v1/feeds", token, "If-None-Match", etag); resp.StatusCode != 304 {
		t.Fatalf("unchanged: %d, want 304", resp.StatusCode)
	}
	feed.Write(feeds, &feed.Feed{Schema: 1, Feed: "host", Status: feed.Warn, UpdatedAt: time.Now().UTC()})
	if resp := get(t, ts.URL+"/v1/feeds", token, "If-None-Match", etag); resp.StatusCode != 200 {
		t.Fatalf("changed: %d, want 200", resp.StatusCode)
	}
}

func TestOneFeed(t *testing.T) {
	ts, token, _ := setup(t)
	if resp := get(t, ts.URL+"/v1/feeds/host", token); resp.StatusCode != 200 {
		t.Fatalf("host: %d", resp.StatusCode)
	}
	if resp := get(t, ts.URL+"/v1/feeds/broken", token); resp.StatusCode != 422 {
		t.Fatalf("broken: %d", resp.StatusCode)
	}
	for _, path := range []string{"/v1/feeds/nope", "/v1/feeds/..%2Fsecret", "/v1/feeds/%2e%2e", "/v1/feeds/host.json", "/secret.json", "/"} {
		if resp := get(t, ts.URL+path, token); resp.StatusCode != 404 {
			t.Errorf("%s: %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestReadOnlyAndHealthz(t *testing.T) {
	ts, token, _ := setup(t)
	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		req, _ := http.NewRequest(method, ts.URL+"/v1/feeds/host", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 405 {
			t.Errorf("%s: %d, want 405", method, resp.StatusCode)
		}
	}
	if resp := get(t, ts.URL+"/healthz", ""); resp.StatusCode != 200 {
		t.Fatalf("healthz: %d", resp.StatusCode)
	}
}
