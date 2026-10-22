// Package feed is the beacon feed format (SPEC.md): types, validation, and reading and
// writing feed files in a feeds folder.
package feed

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	Schema   = 1
	MaxBytes = 256 << 10
	ext      = ".json"
)

type Status string

const (
	OK      Status = "ok"
	Warn    Status = "warn"
	Fail    Status = "fail"
	Unknown Status = "unknown"
)

type Feed struct {
	Schema      int       `json:"schema"`
	Feed        string    `json:"feed"`
	Title       string    `json:"title,omitempty"`
	Status      Status    `json:"status"`
	Summary     string    `json:"summary,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
	ExpectEvery string    `json:"expect_every,omitempty"`
	Metrics     []Metric  `json:"metrics,omitempty"`
	Checks      []Check   `json:"checks,omitempty"`
	Links       []Link    `json:"links,omitempty"`
}

// Labels say which thing a metric or check is about (e.g. endpoint=styles). Metrics with
// the same key and label names form a table in the hub: one row per label value.
type Labels map[string]string

type Metric struct {
	Key     string  `json:"key"`
	Label   string  `json:"label,omitempty"`
	Value   float64 `json:"value"`
	Unit    string  `json:"unit,omitempty"`
	Labels  Labels  `json:"labels,omitempty"`
	Primary bool    `json:"primary,omitempty"`
}

type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
	Labels Labels `json:"labels,omitempty"`
}

type Link struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// SeriesID identifies a metric within its feed: the key plus its labels, sorted
// ("records.new{endpoint=styles}").
func (m Metric) SeriesID() string {
	if len(m.Labels) == 0 {
		return m.Key
	}
	names := make([]string, 0, len(m.Labels))
	for k := range m.Labels {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, k := range names {
		parts[i] = k + "=" + m.Labels[k]
	}
	return m.Key + "{" + strings.Join(parts, ",") + "}"
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidName reports whether s can be a feed name or metric key.
func ValidName(s string) bool { return namePattern.MatchString(s) }

func validStatus(s Status) bool {
	return s == OK || s == Warn || s == Fail || s == Unknown
}

func validLabels(l Labels) error {
	if len(l) > 4 {
		return fmt.Errorf("%d labels, at most 4", len(l))
	}
	for k, v := range l {
		if !ValidName(k) {
			return fmt.Errorf("label name %q is not a valid name", k)
		}
		if v == "" || len([]rune(v)) > 64 || strings.ContainsAny(v, ",={}") {
			return fmt.Errorf("label %s: value must be 1-64 characters without , = { }", k)
		}
	}
	return nil
}

// Validate checks f against SPEC.md and returns every problem found, joined.
func (f *Feed) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	tooLong := func(field, s string, max int) {
		if n := len([]rune(s)); n > max {
			bad("%s: %d characters, at most %d", field, n, max)
		}
	}

	if f.Schema != Schema {
		bad("schema: must be %d", Schema)
	}
	if !ValidName(f.Feed) {
		bad("feed: %q is not a valid name ([a-z0-9][a-z0-9._-]*, at most 64)", f.Feed)
	}
	tooLong("title", f.Title, 100)
	if !validStatus(f.Status) {
		bad("status: must be ok, warn, fail or unknown")
	}
	tooLong("summary", f.Summary, 500)
	if f.UpdatedAt.IsZero() {
		bad("updated_at: required")
	}
	if f.ExpectEvery != "" {
		if d, err := time.ParseDuration(f.ExpectEvery); err != nil || d <= 0 {
			bad("expect_every: %q is not a positive duration (e.g. 5m, 26h)", f.ExpectEvery)
		}
	}
	if len(f.Metrics) > 100 {
		bad("metrics: %d, at most 100", len(f.Metrics))
	}
	seen := map[string]bool{}
	primary := 0
	for i, m := range f.Metrics {
		at := fmt.Sprintf("metrics[%d]", i)
		if err := validLabels(m.Labels); err != nil {
			bad("%s.labels: %v", at, err)
		}
		if !ValidName(m.Key) {
			bad("%s.key: %q is not a valid key", at, m.Key)
		} else if seen[m.SeriesID()] {
			bad("%s: %s appears twice", at, m.SeriesID())
		}
		seen[m.SeriesID()] = true
		if m.Primary {
			primary++
		}
		if math.IsNaN(m.Value) || math.IsInf(m.Value, 0) {
			bad("%s.value: must be a finite number", at)
		}
		tooLong(at+".label", m.Label, 100)
		tooLong(at+".unit", m.Unit, 16)
	}
	if primary > 6 {
		bad("metrics: %d marked primary, at most 6", primary)
	}
	if len(f.Checks) > 50 {
		bad("checks: %d, at most 50", len(f.Checks))
	}
	for i, c := range f.Checks {
		at := fmt.Sprintf("checks[%d]", i)
		if strings.TrimSpace(c.Name) == "" {
			bad("%s.name: required", at)
		}
		tooLong(at+".name", c.Name, 100)
		if !validStatus(c.Status) {
			bad("%s.status: must be ok, warn, fail or unknown", at)
		}
		tooLong(at+".detail", c.Detail, 500)
		if err := validLabels(c.Labels); err != nil {
			bad("%s.labels: %v", at, err)
		}
	}
	if len(f.Links) > 10 {
		bad("links: %d, at most 10", len(f.Links))
	}
	for i, l := range f.Links {
		at := fmt.Sprintf("links[%d]", i)
		if strings.TrimSpace(l.Title) == "" {
			bad("%s.title: required", at)
		}
		tooLong(at+".title", l.Title, 100)
		if !(strings.HasPrefix(l.URL, "https://") || strings.HasPrefix(l.URL, "http://")) || len(l.URL) > 500 || strings.ContainsAny(l.URL, " \t\n") {
			bad("%s.url: must be an http(s) URL of at most 500 characters", at)
		}
	}
	return errors.Join(errs...)
}

// Parse decodes and validates one feed document. Unknown fields are an error.
func Parse(data []byte) (*Feed, error) {
	if len(data) > MaxBytes {
		return nil, fmt.Errorf("%d bytes, at most %d", len(data), MaxBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f Feed
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("not a feed document: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("not a feed document: trailing data after the JSON object")
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// Write validates f and writes it to dir/<feed>.json atomically (temp file + rename),
// so readers never see a half-written feed.
func Write(dir string, f *Feed) error {
	if err := f.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > MaxBytes {
		return fmt.Errorf("feed %s: %d bytes, at most %d", f.Feed, len(data), MaxBytes)
	}
	tmp, err := os.CreateTemp(dir, "."+f.Feed+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, f.Feed+ext))
}

// Load reads one feed file by name. ok is false when there is no such feed.
func Load(dir, name string) (f *Feed, ok bool, err error) {
	if !ValidName(name) {
		return nil, false, nil
	}
	data, ok, err := readRegular(filepath.Join(dir, name+ext))
	if !ok || err != nil {
		return nil, ok, err
	}
	f, err = Parse(data)
	if err == nil && f.Feed != name {
		err = fmt.Errorf("feed: %q doesn't match the file name", f.Feed)
	}
	return f, true, err
}

// Invalid is a file in the feeds folder that isn't a valid feed.
type Invalid struct {
	File  string `json:"file"`
	Error string `json:"error"`
}

// LoadAll reads every feed in dir, sorted by name. Only regular *.json files directly in
// dir count; anything else (subfolders, symlinks, temp files) is ignored.
func LoadAll(dir string) ([]*Feed, []Invalid, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	var feeds []*Feed
	var invalid []Invalid
	for _, e := range entries {
		file := e.Name()
		name, isJSON := strings.CutSuffix(file, ext)
		if !isJSON || !e.Type().IsRegular() || strings.HasPrefix(file, ".") {
			continue
		}
		if !ValidName(name) {
			invalid = append(invalid, Invalid{file, "file name is not a valid feed name"})
			continue
		}
		f, ok, err := Load(dir, name)
		switch {
		case err != nil:
			invalid = append(invalid, Invalid{file, err.Error()})
		case ok:
			feeds = append(feeds, f)
		}
	}
	sort.Slice(feeds, func(i, j int) bool { return feeds[i].Feed < feeds[j].Feed })
	return feeds, invalid, nil
}

// readRegular reads path if it is a regular file (not a symlink) of at most MaxBytes.
func readRegular(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, nil
	}
	if info.Size() > MaxBytes {
		return nil, true, fmt.Errorf("%d bytes, at most %d", info.Size(), MaxBytes)
	}
	// O_NOFOLLOW: a symlink swapped in after the Lstat still isn't followed.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, true, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxBytes+1))
	return data, true, err
}
