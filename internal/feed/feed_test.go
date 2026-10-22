package feed

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sample() *Feed {
	return &Feed{
		Schema: Schema, Feed: "centric-api", Title: "Centric fetch", Status: OK,
		UpdatedAt: time.Date(2026, 10, 22, 1, 1, 12, 0, time.UTC), ExpectEvery: "26h",
		Metrics: []Metric{{Key: "styles.new", Value: 12, Unit: "count"}},
		Checks:  []Check{{Name: "endpoints fresh", Status: OK}},
	}
}

func TestWriteThenLoadRoundTrips(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, sample()); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Load(dir, "centric-api")
	if err != nil || !ok {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
	}
	if got.Metrics[0].Value != 12 || got.ExpectEvery != "26h" || !got.UpdatedAt.Equal(sample().UpdatedAt) {
		t.Fatalf("round trip changed the feed: %+v", got)
	}
	info, _ := os.Stat(filepath.Join(dir, "centric-api.json"))
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v, want 0644", info.Mode().Perm())
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".*.tmp")); len(leftovers) > 0 {
		t.Fatalf("temp files left: %v", leftovers)
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	f := sample()
	f.Schema, f.Feed, f.Status, f.ExpectEvery, f.Visibility = 2, "Bad Name", "great", "soon", "secret"
	f.Metrics = append(f.Metrics, Metric{Key: "styles.new", Value: 1})
	f.Checks = append(f.Checks, Check{Name: " ", Status: "meh"})
	err := f.Validate()
	if err == nil {
		t.Fatal("want errors")
	}
	for _, want := range []string{"schema", "feed:", "status:", "expect_every", "visibility", "appears twice", "checks[1].name", "checks[1].status"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error doesn't mention %q:\n%v", want, err)
		}
	}
}

func TestParseRejectsUnknownFieldsAndTrailingData(t *testing.T) {
	base := `{"schema":1,"feed":"x","status":"ok","updated_at":"2026-10-22T00:00:00Z"`
	if _, err := Parse([]byte(base + `}`)); err != nil {
		t.Fatalf("minimal feed: %v", err)
	}
	if _, err := Parse([]byte(base + `,"stauts":"ok"}`)); err == nil {
		t.Fatal("unknown field accepted")
	}
	if _, err := Parse([]byte(base + `}{}`)); err == nil {
		t.Fatal("trailing data accepted")
	}
	if _, err := Parse([]byte(base + `,}`)); err == nil {
		t.Fatal("broken JSON accepted")
	}
}

func TestLoadAllReadsOnlyRegularFeedFiles(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, sample()); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "secret.json")
	os.WriteFile(secret, []byte(`{"schema":1,"feed":"link","status":"ok","updated_at":"2026-10-22T00:00:00Z"}`), 0o644)
	os.Symlink(secret, filepath.Join(dir, "link.json"))
	os.Mkdir(filepath.Join(dir, "sub.json"), 0o755)
	os.WriteFile(filepath.Join(dir, ".hidden.json"), []byte(`{}`), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte(`hi`), 0o644)
	os.WriteFile(filepath.Join(dir, "broken.json"), []byte(`{"schema":1}`), 0o644)
	os.WriteFile(filepath.Join(dir, "Upper.json"), []byte(`{}`), 0o644)
	os.WriteFile(filepath.Join(dir, "other.json"),
		[]byte(`{"schema":1,"feed":"not-other","status":"ok","updated_at":"2026-10-22T00:00:00Z"}`), 0o644)

	feeds, invalid, err := LoadAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(feeds) != 1 || feeds[0].Feed != "centric-api" {
		t.Fatalf("feeds = %+v, want only centric-api", feeds)
	}
	files := map[string]bool{}
	for _, inv := range invalid {
		files[inv.File] = true
	}
	for _, want := range []string{"broken.json", "Upper.json", "other.json"} {
		if !files[want] {
			t.Errorf("%s not reported invalid: %+v", want, invalid)
		}
	}
	if files["link.json"] || files[".hidden.json"] || files["sub.json"] {
		t.Errorf("non-regular or hidden files reported: %+v", invalid)
	}
	if _, ok, _ := Load(dir, "link"); ok {
		t.Fatal("Load followed a symlink")
	}
	if _, ok, _ := Load(dir, "../secret"); ok {
		t.Fatal("Load accepted a path")
	}
}

func TestWriteRefusesInvalidFeeds(t *testing.T) {
	f := sample()
	f.Status = ""
	if err := Write(t.TempDir(), f); err == nil {
		t.Fatal("invalid feed written")
	}
}
