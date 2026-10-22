// Package spec is beacon v2's data (SPEC.md): components (state), runs and events (the
// log), their validation, and reading/writing component files.
package spec

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
	Schema   = 2
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

// Labels say which of several things a metric or check is about (endpoint=styles).
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

type Schedule struct {
	NextAt *time.Time `json:"next_at,omitempty"`
	Grace  string     `json:"grace,omitempty"`
	Every  string     `json:"every,omitempty"`
}

type Running struct {
	ID        string    `json:"id,omitempty"`
	StartedAt time.Time `json:"started_at"`
	Trigger   string    `json:"trigger,omitempty"`
}

// Component is the state of one thing on the box (SPEC.md → Component).
type Component struct {
	ID          string    `json:"id"`
	Title       string    `json:"title,omitempty"`
	Group       string    `json:"group,omitempty"`
	Description string    `json:"description,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
	ExpectEvery string    `json:"expect_every,omitempty"`
	Schedule    *Schedule `json:"schedule,omitempty"`
	Running     *Running  `json:"running,omitempty"`
	Status      Status    `json:"status,omitempty"`
	Summary     string    `json:"summary,omitempty"`
	Gauges      []Metric  `json:"gauges,omitempty"`
	Checks      []Check   `json:"checks,omitempty"`
	Links       []Link    `json:"links,omitempty"`
}

type Outcome string

const (
	Success   Outcome = "success"
	Failure   Outcome = "failure"
	Cancelled Outcome = "cancelled"
)

// Run is one execution of a component's job (a log entry).
type Run struct {
	Seq       int64     `json:"seq,omitempty"`
	Type      string    `json:"type"` // "run"
	Component string    `json:"component"`
	ID        string    `json:"id"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Outcome   Outcome   `json:"outcome"`
	Trigger   string    `json:"trigger,omitempty"`
	ExitCode  *int      `json:"exit_code,omitempty"`
	Unit      string    `json:"unit,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	Stats     []Metric  `json:"stats,omitempty"`
	Error     string    `json:"error,omitempty"`
	Artifacts []Link    `json:"artifacts,omitempty"`
}

type EventKind string

// Event is a reboot, deploy, config change or note (a log entry).
type Event struct {
	Seq       int64     `json:"seq,omitempty"`
	Type      string    `json:"type"` // "event"
	ID        string    `json:"id"`
	At        time.Time `json:"at"`
	Kind      EventKind `json:"kind"`
	Component string    `json:"component,omitempty"`
	Title     string    `json:"title"`
	Detail    string    `json:"detail,omitempty"`
}

var (
	namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	idPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
	Triggers    = map[string]bool{"timer": true, "manual": true, "boot": true, "dependency": true, "other": true}
	Kinds       = map[EventKind]bool{"reboot": true, "deploy": true, "config": true, "note": true}
)

// ValidName reports whether s can be a component id, metric key or label name.
func ValidName(s string) bool { return namePattern.MatchString(s) }

func validStatus(s Status) bool { return s == OK || s == Warn || s == Fail || s == Unknown }

// SeriesID is a metric's identity in its list: key plus sorted labels.
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

// ParseDuration accepts Go durations ("90s", "26h", "1h30m"), positive.
func ParseDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%q is not a positive duration (e.g. 5m, 26h)", s)
	}
	return d, nil
}

type problems struct{ errs []error }

func (p *problems) add(format string, args ...any) {
	p.errs = append(p.errs, fmt.Errorf(format, args...))
}
func (p *problems) long(field, s string, max int) {
	if n := len([]rune(s)); n > max {
		p.add("%s: %d characters, at most %d", field, n, max)
	}
}
func (p *problems) err() error { return errors.Join(p.errs...) }

func (p *problems) labels(at string, l Labels) {
	if len(l) > 4 {
		p.add("%s.labels: %d, at most 4", at, len(l))
	}
	for k, v := range l {
		if !ValidName(k) {
			p.add("%s.labels: %q is not a valid name", at, k)
		}
		if v == "" || len([]rune(v)) > 64 || strings.ContainsAny(v, ",={}") {
			p.add("%s.labels.%s: value must be 1-64 characters without , = { }", at, k)
		}
	}
}

func (p *problems) metrics(field string, ms []Metric) {
	if len(ms) > 100 {
		p.add("%s: %d, at most 100", field, len(ms))
	}
	seen, primary := map[string]bool{}, 0
	for i, m := range ms {
		at := fmt.Sprintf("%s[%d]", field, i)
		if !ValidName(m.Key) {
			p.add("%s.key: %q is not a valid key", at, m.Key)
		} else if seen[m.SeriesID()] {
			p.add("%s: %s appears twice", at, m.SeriesID())
		}
		seen[m.SeriesID()] = true
		if math.IsNaN(m.Value) || math.IsInf(m.Value, 0) {
			p.add("%s.value: must be a finite number", at)
		}
		p.long(at+".label", m.Label, 100)
		p.long(at+".unit", m.Unit, 16)
		p.labels(at, m.Labels)
		if m.Primary {
			primary++
		}
	}
	if primary > 6 {
		p.add("%s: %d marked primary, at most 6", field, primary)
	}
}

func (p *problems) checks(cs []Check) {
	if len(cs) > 50 {
		p.add("checks: %d, at most 50", len(cs))
	}
	for i, c := range cs {
		at := fmt.Sprintf("checks[%d]", i)
		if strings.TrimSpace(c.Name) == "" {
			p.add("%s.name: required", at)
		}
		p.long(at+".name", c.Name, 100)
		if !validStatus(c.Status) {
			p.add("%s.status: must be ok, warn, fail or unknown", at)
		}
		p.long(at+".detail", c.Detail, 500)
		p.labels(at, c.Labels)
	}
}

func (p *problems) links(field string, ls []Link) {
	if len(ls) > 10 {
		p.add("%s: %d, at most 10", field, len(ls))
	}
	for i, l := range ls {
		at := fmt.Sprintf("%s[%d]", field, i)
		if strings.TrimSpace(l.Title) == "" {
			p.add("%s.title: required", at)
		}
		p.long(at+".title", l.Title, 100)
		if !(strings.HasPrefix(l.URL, "https://") || strings.HasPrefix(l.URL, "http://")) || len(l.URL) > 500 || strings.ContainsAny(l.URL, " \t\n") {
			p.add("%s.url: must be an http(s) URL of at most 500 characters", at)
		}
	}
}

func (c *Component) Validate() error {
	var p problems
	if !ValidName(c.ID) {
		p.add("id: %q is not a valid id ([a-z0-9][a-z0-9._-]*, at most 64)", c.ID)
	}
	p.long("title", c.Title, 100)
	p.long("group", c.Group, 50)
	p.long("description", c.Description, 300)
	if c.UpdatedAt.IsZero() {
		p.add("updated_at: required")
	}
	if c.ExpectEvery != "" {
		if _, err := ParseDuration(c.ExpectEvery); err != nil {
			p.add("expect_every: %v", err)
		}
	}
	if s := c.Schedule; s != nil {
		if s.Grace != "" {
			if _, err := ParseDuration(s.Grace); err != nil {
				p.add("schedule.grace: %v", err)
			}
		}
		p.long("schedule.every", s.Every, 100)
	}
	if r := c.Running; r != nil {
		if r.StartedAt.IsZero() {
			p.add("running.started_at: required")
		}
		if r.Trigger != "" && !Triggers[r.Trigger] {
			p.add("running.trigger: %q is not one of timer, manual, boot, dependency, other", r.Trigger)
		}
	}
	if c.Status != "" && !validStatus(c.Status) {
		p.add("status: must be ok, warn, fail or unknown")
	}
	p.long("summary", c.Summary, 500)
	p.metrics("gauges", c.Gauges)
	p.checks(c.Checks)
	p.links("links", c.Links)
	return p.err()
}

func (r *Run) Validate() error {
	var p problems
	if r.Type != "run" {
		p.add(`type: must be "run"`)
	}
	if !ValidName(r.Component) {
		p.add("component: %q is not a valid id", r.Component)
	}
	if !idPattern.MatchString(r.ID) {
		p.add("id: %q is not a valid run id", r.ID)
	}
	if r.StartedAt.IsZero() || r.EndedAt.IsZero() {
		p.add("started_at, ended_at: required")
	} else if r.EndedAt.Before(r.StartedAt) {
		p.add("ended_at: before started_at")
	}
	if r.Outcome != Success && r.Outcome != Failure && r.Outcome != Cancelled {
		p.add("outcome: must be success, failure or cancelled")
	}
	if r.Trigger != "" && !Triggers[r.Trigger] {
		p.add("trigger: %q is not one of timer, manual, boot, dependency, other", r.Trigger)
	}
	p.long("unit", r.Unit, 200)
	p.long("summary", r.Summary, 500)
	p.long("error", r.Error, 2000)
	p.metrics("stats", r.Stats)
	p.links("artifacts", r.Artifacts)
	return p.err()
}

func (e *Event) Validate() error {
	var p problems
	if e.Type != "event" {
		p.add(`type: must be "event"`)
	}
	if !idPattern.MatchString(e.ID) {
		p.add("id: %q is not a valid event id", e.ID)
	}
	if e.At.IsZero() {
		p.add("at: required")
	}
	if !Kinds[e.Kind] {
		p.add("kind: must be reboot, deploy, config or note")
	}
	if e.Component != "" && !ValidName(e.Component) {
		p.add("component: %q is not a valid id", e.Component)
	}
	if strings.TrimSpace(e.Title) == "" {
		p.add("title: required")
	}
	p.long("title", e.Title, 200)
	p.long("detail", e.Detail, 2000)
	return p.err()
}

// Decode reads exactly one JSON object into v; unknown fields are an error.
func Decode(data []byte, v any) error {
	if len(data) > MaxBytes {
		return fmt.Errorf("%d bytes, at most %d", len(data), MaxBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("not a valid document: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("not a valid document: trailing data after the JSON object")
	}
	return nil
}

// ParseComponent decodes and validates a component document.
func ParseComponent(data []byte) (*Component, error) {
	var c Component
	if err := Decode(data, &c); err != nil {
		return nil, err
	}
	return &c, c.Validate()
}

// WriteComponent validates c and writes dir/<id>.json atomically.
func WriteComponent(dir string, c *Component) error {
	if err := c.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(filepath.Join(dir, c.ID+ext), append(data, '\n'), 0o644)
}

// WriteFileAtomic writes via a temp file + rename, so readers never see half a file.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Invalid is a file that isn't a valid component.
type Invalid struct {
	File  string `json:"file"`
	Error string `json:"error"`
}

// LoadComponents reads every component in dir, sorted by id: regular *.json files directly
// in dir (no subfolders, symlinks or dotfiles).
func LoadComponents(dir string) ([]*Component, []Invalid, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var comps []*Component
	var invalid []Invalid
	for _, e := range entries {
		file := e.Name()
		id, isJSON := strings.CutSuffix(file, ext)
		if !isJSON || !e.Type().IsRegular() || strings.HasPrefix(file, ".") {
			continue
		}
		data, err := readRegular(filepath.Join(dir, file))
		var c *Component
		if err == nil {
			c, err = ParseComponent(data)
		}
		if err == nil && c.ID != id {
			err = fmt.Errorf("id %q doesn't match the file name", c.ID)
		}
		if err != nil {
			invalid = append(invalid, Invalid{file, err.Error()})
			continue
		}
		comps = append(comps, c)
	}
	sort.Slice(comps, func(i, j int) bool { return comps[i].ID < comps[j].ID })
	return comps, invalid, nil
}

// LoadComponent reads one component file; ok is false when there's none.
func LoadComponent(dir, id string) (c *Component, ok bool, err error) {
	if !ValidName(id) {
		return nil, false, nil
	}
	data, err := readRegular(filepath.Join(dir, id+ext))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	c, err = ParseComponent(data)
	return c, true, err
}

// readRegular reads path if it's a regular file (not a symlink) of at most MaxBytes.
func readRegular(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", filepath.Base(path))
	}
	if info.Size() > MaxBytes {
		return nil, fmt.Errorf("%d bytes, at most %d", info.Size(), MaxBytes)
	}
	// O_NOFOLLOW: a symlink swapped in after the Lstat still isn't followed.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, MaxBytes+1))
}

// SetSeq is how the log numbers an entry (logbook.Entry).
func (r *Run) SetSeq(n int64)   { r.Seq = n }
func (e *Event) SetSeq(n int64) { e.Seq = n }
