// Package collect runs collector scripts: each executable in the collectors folder
// prints one feed document on stdout, which is validated and written to the feeds folder.
package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/eemax/beacon/internal/feed"
)

// Result is what happened to one collector.
type Result struct {
	Collector string
	Feed      string
	Err       error
}

// Run runs every executable regular file in dir (sorted, dotfiles skipped), each with
// timeout. A collector's feed name defaults to its file name without extension. When a
// collector fails, its feed is written as status "fail" with the reason, so the hub sees
// the failure instead of the feed just going stale.
func Run(ctx context.Context, dir, feedsDir string, timeout time.Duration, now func() time.Time) ([]Result, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var results []Result
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 ||
			strings.HasPrefix(e.Name(), ".") {
			continue
		}
		results = append(results, runOne(ctx, filepath.Join(dir, e.Name()), feedsDir, timeout, now))
	}
	return results, nil
}

func runOne(ctx context.Context, path, feedsDir string, timeout time.Duration, now func() time.Time) Result {
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	res := Result{Collector: filepath.Base(path), Feed: name}
	f, err := execute(ctx, path, name, timeout, now)
	if err == nil {
		res.Feed = f.Feed
		res.Err = feed.Write(feedsDir, f)
		return res
	}
	res.Err = err
	if !feed.ValidName(name) {
		return res
	}
	// Keep the title and expected cadence of the last good run, so the failure looks
	// like the same feed failing rather than a new one.
	failed := &feed.Feed{
		Schema: feed.Schema, Feed: name, Status: feed.Fail, UpdatedAt: now().UTC(),
		Summary: truncate("collector failed: "+err.Error(), 500),
	}
	if prev, ok, _ := feed.Load(feedsDir, name); ok && prev != nil {
		failed.Title, failed.ExpectEvery, failed.Visibility = prev.Title, prev.ExpectEvery, prev.Visibility
	}
	if werr := feed.Write(feedsDir, failed); werr != nil {
		res.Err = fmt.Errorf("%w (and writing the failure: %v)", err, werr)
	}
	return res
}

func execute(ctx context.Context, path, name string, timeout time.Duration, now func() time.Time) (*feed.Feed, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path)
	cmd.Env = append(os.Environ(), "BEACON_FEED="+name)
	// Own process group, killed as a whole on timeout: a script's children (curl, sleep…)
	// mustn't outlive it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("timed out after %s", timeout)
		}
		msg := strings.TrimSpace(lastLine(stderr.String()))
		if msg == "" {
			return nil, err
		}
		return nil, fmt.Errorf("%v: %s", err, msg)
	}
	return FromJSON(stdout.Bytes(), name, now)
}

// FromJSON turns a producer's document into a feed: schema, feed name and updated_at
// are filled in when missing, then everything is validated.
func FromJSON(data []byte, name string, now func() time.Time) (*feed.Feed, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("output is not a JSON object: %w", err)
	}
	defaults := map[string]any{"schema": feed.Schema, "feed": name, "updated_at": now().UTC()}
	for key, value := range defaults {
		if _, set := probe[key]; !set {
			raw, _ := json.Marshal(value)
			probe[key] = raw
		}
	}
	filled, _ := json.Marshal(probe)
	return feed.Parse(filled)
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func truncate(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}
