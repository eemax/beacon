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
		Schema: Schema, Feed: "nightly-sync", Title: "Nightly sync", Status: OK,
		UpdatedAt: time.Date(2026, 10, 22, 1, 1, 12, 0, time.UTC), ExpectEvery: "26h",
		Metrics: []Metric{{Key: "records.new", Value: 12, Unit: "count"}},
		Checks:  []Check{{Name: "endpoints fresh", Status: OK}},
	}
}

func TestWriteThenLoadRoundTrips(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, sample()); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Load(dir, "nightly-sync")
	if err != nil || !ok {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
	}
	if got.Metrics[0].Value != 12 || got.ExpectEvery != "26h" || !got.UpdatedAt.Equal(sample().UpdatedAt) {
		t.Fatalf("round trip changed the feed: %+v", got)
	}
	info, _ := os.Stat(filepath.Join(dir, "nightly-sync.json"))
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v, want 0644", info.Mode().Perm())
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".*.tmp")); len(leftovers) > 0 {
		t.Fatalf("temp files left: %v", leftovers)
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	f := sample()
	f.Schema, f.Feed, f.Status, f.ExpectEvery = 2, "Bad Name", "great", "soon"
	f.Metrics = append(f.Metrics, Metric{Key: "records.new", Value: 1})
	f.Metrics = append(f.Metrics, Metric{Key: "x", Value: 1, Labels: Labels{"Bad": "v"}})
	f.Checks = append(f.Checks, Check{Name: " ", Status: "meh", Labels: Labels{"endpoint": "a,b"}})
	f.Links = []Link{{Title: "", URL: "ftp://x"}}
	err := f.Validate()
	if err == nil {
		t.Fatal("want errors")
	}
	for _, want := range []string{"schema", "feed:", "status:", "expect_every", "appears twice",
		"metrics[2].labels", "checks[1].name", "checks[1].status", "checks[1].labels", "links[0].title", "links[0].url"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error doesn't mention %q:\n%v", want, err)
		}
	}
}

func TestLabelsMakeSeriesDistinct(t *testing.T) {
	f := sample()
	f.Metrics = []Metric{
		{Key: "records.new", Value: 1, Labels: Labels{"endpoint": "styles"}},
		{Key: "records.new", Value: 2, Labels: Labels{"endpoint": "boms"}},
		{Key: "records.new", Value: 3},
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("same key, different labels: %v", err)
	}
	m := Metric{Key: "k", Labels: Labels{"b": "2", "a": "1"}}
	if got := m.SeriesID(); got != "k{a=1,b=2}" {
		t.Fatalf("SeriesID = %q", got)
	}
	f.Metrics = append(f.Metrics, Metric{Key: "records.new", Value: 4, Labels: Labels{"endpoint": "boms"}})
	if err := f.Validate(); err == nil || !strings.Contains(err.Error(), "records.new{endpoint=boms} appears twice") {
		t.Fatalf("duplicate series accepted: %v", err)
	}
}

func TestAtMostSixPrimaryMetrics(t *testing.T) {
	f := sample()
	f.Metrics = nil
	for i := 0; i < 7; i++ {
		f.Metrics = append(f.Metrics, Metric{Key: "m" + string(rune('a'+i)), Value: 1, Primary: true})
	}
	if err := f.Validate(); err == nil || !strings.Contains(err.Error(), "primary") {
		t.Fatalf("7 primary metrics accepted: %v", err)
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
	if len(feeds) != 1 || feeds[0].Feed != "nightly-sync" {
		t.Fatalf("feeds = %+v, want only nightly-sync", feeds)
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
