package collect

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eemax/beacon/internal/feed"
)

func script(t *testing.T, dir, name, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), mode); err != nil {
		t.Fatal(err)
	}
}

func TestRunWritesFeedsAndFailures(t *testing.T) {
	collectors, feeds := t.TempDir(), t.TempDir()
	script(t, collectors, "good.sh", `echo '{"status":"ok","title":"Good","metrics":[{"key":"n","value":3}]}'`, 0o755)
	script(t, collectors, "named", `echo '{"feed":"custom","status":"warn"}'`, 0o755)
	script(t, collectors, "bad.sh", `echo 'boom' >&2; exit 3`, 0o755)
	script(t, collectors, "junk.sh", `echo 'not json'`, 0o755)
	script(t, collectors, "slow.sh", `sleep 5`, 0o755)
	script(t, collectors, "off.sh", `echo '{"status":"ok"}'`, 0o644) // not executable: skipped
	script(t, collectors, ".hidden.sh", `echo '{"status":"ok"}'`, 0o755)
	// bad.sh had a good run before: its failure keeps that title and cadence.
	feed.Write(feeds, &feed.Feed{Schema: 1, Feed: "bad", Title: "Nightly bad", ExpectEvery: "26h", Status: feed.OK, UpdatedAt: time.Now()})

	results, err := Run(context.Background(), collectors, feeds, 500*time.Millisecond, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	errs := map[string]error{}
	for _, r := range results {
		errs[r.Collector] = r.Err
	}
	if len(results) != 5 || errs["good.sh"] != nil || errs["named"] != nil {
		t.Fatalf("results = %+v", results)
	}
	for _, name := range []string{"bad.sh", "junk.sh", "slow.sh"} {
		if errs[name] == nil {
			t.Errorf("%s should have failed", name)
		}
	}

	good, _, _ := feed.Load(feeds, "good")
	if good.Title != "Good" || good.Schema != 1 || good.UpdatedAt.IsZero() || good.Metrics[0].Value != 3 {
		t.Fatalf("good = %+v", good)
	}
	if custom, ok, _ := feed.Load(feeds, "custom"); !ok || custom.Status != feed.Warn {
		t.Fatalf("custom = %+v", custom)
	}
	bad, _, _ := feed.Load(feeds, "bad")
	if bad.Status != feed.Fail || bad.Title != "Nightly bad" || bad.ExpectEvery != "26h" || !strings.Contains(bad.Summary, "boom") {
		t.Fatalf("bad = %+v", bad)
	}
	slow, _, _ := feed.Load(feeds, "slow")
	if slow.Status != feed.Fail || !strings.Contains(slow.Summary, "timed out") {
		t.Fatalf("slow = %+v", slow)
	}
	if _, ok, _ := feed.Load(feeds, "off"); ok {
		t.Fatal("non-executable collector ran")
	}
}

func TestMissingCollectorsFolderIsFine(t *testing.T) {
	results, err := Run(context.Background(), filepath.Join(t.TempDir(), "none"), t.TempDir(), time.Second, time.Now)
	if err != nil || len(results) != 0 {
		t.Fatalf("results=%v err=%v", results, err)
	}
}
