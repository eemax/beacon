// Package spec is beacon v3's data (SPEC.md): components (state), runs and events (the
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
	"unicode"
)

const (
	Schema   = 3
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

// Class says what a component is for, and so what a hub makes of its status: a service
// failing needs someone today; an advisory or a list only ever suggests.
type Class string

const (
	Service  Class = "service"
	Advisory Class = "advisory"
	List     Class = "list"
)

func validClass(c Class) bool { return c == Service || c == Advisory || c == List }

// Item is one thing in a component's list: work, a suggestion, something waiting. Items are
// never judged by a hub; they're shown.
type Item struct {
	ID      string     `json:"id"`
	Title   string     `json:"title"`
	Since   *time.Time `json:"since,omitempty"`
	Status  Status     `json:"status,omitempty"` // ok or warn
	URL     string     `json:"url,omitempty"`
	Waiting *bool      `json:"waiting,omitempty"` // true, or left out
	Detail  string     `json:"detail,omitempty"`
}

// MaxItems is the most items a component lists.
const MaxItems = 200

// Reporting says the component's state adapter is failing: the rest of the component is
// the adapter's last good output, and LastGoodAt (null when it never was good) says how
// old that is. Only beacon sets it.
type Reporting struct {
	Error      string     `json:"error"`
	Since      time.Time  `json:"since"`
	LastGoodAt *time.Time `json:"last_good_at"`
}

// UnmarshalJSON insists on all three fields (last_good_at may be null, not missing).
func (r *Reporting) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for k := range raw {
		if k != "error" && k != "since" && k != "last_good_at" {
			return fmt.Errorf("reporting: unknown field %q", k)
		}
	}
	if _, ok := raw["last_good_at"]; !ok {
		return errors.New("reporting.last_good_at: required (null when it was never good)")
	}
	type plain Reporting
	return json.Unmarshal(data, (*plain)(r))
}

// Component is the state of one thing on the box (SPEC.md → Component).
type Component struct {
	ID          string     `json:"id"`
	Class       Class      `json:"class,omitempty"`
	Title       string     `json:"title,omitempty"`
	Group       string     `json:"group,omitempty"`
	Description string     `json:"description,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ExpectEvery string     `json:"expect_every,omitempty"`
	Schedule    *Schedule  `json:"schedule,omitempty"`
	Running     *Running   `json:"running,omitempty"`
	Status      Status     `json:"status,omitempty"`
	Summary     string     `json:"summary,omitempty"`
	Gauges      []Metric   `json:"gauges,omitempty"`
	Checks      []Check    `json:"checks,omitempty"`
	Links       []Link     `json:"links,omitempty"`
	Items       []Item     `json:"items,omitempty"`
	Reporting   *Reporting `json:"reporting,omitempty"`
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

// Actor is who made a change: a person, an agent (an AI agent or automation acting for
// someone) or a unit (a systemd unit or job).
type Actor struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// Ref names a thing that lives elsewhere and has no URL of its own (a commit, an alert).
type Ref struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// EventLink is what an event points to: a URL or a ref, exactly one of them.
type EventLink struct {
	Title string `json:"title"`
	URL   string `json:"url,omitempty"`
	Ref   *Ref   `json:"ref,omitempty"`
}

// Event is a reboot, deploy, config change, other change, note, or a service that crashed
// or failed (a log entry).
type Event struct {
	Seq       int64       `json:"seq,omitempty"`
	Type      string      `json:"type"` // "event"
	ID        string      `json:"id"`
	At        time.Time   `json:"at"`
	Kind      EventKind   `json:"kind"`
	Component string      `json:"component,omitempty"`
	Title     string      `json:"title"`
	Detail    string      `json:"detail,omitempty"`
	Actor     *Actor      `json:"actor,omitempty"`
	Links     []EventLink `json:"links,omitempty"`
}

var (
	namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	idPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
	Triggers    = map[string]bool{"timer": true, "manual": true, "boot": true, "dependency": true, "other": true}
	Kinds       = map[EventKind]bool{"reboot": true, "deploy": true, "config": true, "note": true, "service": true, "change": true}
	ActorKinds  = map[string]bool{"person": true, "agent": true, "unit": true}
	RefKinds    = map[string]bool{"commit": true, "alert": true, "incident": true, "conversation": true, "other": true}
)

const (
	MaxEventLinks = 10
	maxActorName  = 100
	maxRefID      = 200
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
		p.url(at, l.URL)
	}
}

func (p *problems) items(items []Item) {
	if len(items) > MaxItems {
		p.add("items: %d, at most %d", len(items), MaxItems)
	}
	seen := map[string]bool{}
	for i, it := range items {
		at := fmt.Sprintf("items[%d]", i)
		switch n := len([]rune(it.ID)); {
		case n == 0 || n > 64:
			p.add("%s.id: 1-64 characters", at)
		case strings.IndexFunc(it.ID, unicode.IsControl) >= 0:
			p.add("%s.id: no control characters", at)
		case seen[it.ID]:
			p.add("%s.id: %q appears twice", at, it.ID)
		}
		seen[it.ID] = true
		if strings.TrimSpace(it.Title) == "" {
			p.add("%s.title: required", at)
		}
		p.long(at+".title", it.Title, 200)
		if it.Status != "" && it.Status != OK && it.Status != Warn {
			p.add("%s.status: must be ok or warn", at)
		}
		if it.URL != "" {
			p.url(at, it.URL)
		}
		if it.Waiting != nil && !*it.Waiting {
			p.add("%s.waiting: true, or leave it out", at)
		}
		p.long(at+".detail", it.Detail, 500)
	}
}

// url checks an http(s) URL with a host, at most 500 characters (code points, like
// every length in SPEC).
func (p *problems) url(at, u string) {
	if !(strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")) || len([]rune(u)) > 500 ||
		strings.ContainsAny(u, " \t\n") || !hasHost(u) {
		p.add("%s.url: must be an http(s) URL with a host, of at most 500 characters", at)
	}
}

// hasHost reports whether an http(s) URL names a host: the authority after "://" (up to
// the first / ? or #), past any userinfo@, is neither empty nor only a :port. Deliberately
// not url.Parse: links beacon v2.5.0 accepted (an unencoded %, a bad port) stay valid.
func hasHost(u string) bool {
	_, rest, _ := strings.Cut(u, "://")
	authority := rest
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authority = rest[:i]
	}
	if i := strings.LastIndex(authority, "@"); i >= 0 {
		authority = authority[i+1:]
	}
	return authority != "" && !strings.HasPrefix(authority, ":")
}

func (p *problems) actor(a *Actor) {
	if a == nil {
		return
	}
	if !ActorKinds[a.Kind] {
		p.add("actor.kind: %q is not one of person, agent, unit", a.Kind)
	}
	if strings.TrimSpace(a.Name) == "" {
		p.add("actor.name: required")
	}
	p.long("actor.name", a.Name, maxActorName)
}

func (p *problems) eventLinks(ls []EventLink) {
	if len(ls) > MaxEventLinks {
		p.add("links: %d, at most %d", len(ls), MaxEventLinks)
	}
	for i, l := range ls {
		at := fmt.Sprintf("links[%d]", i)
		if strings.TrimSpace(l.Title) == "" {
			p.add("%s.title: required", at)
		}
		p.long(at+".title", l.Title, 100)
		switch {
		case l.URL != "" && l.Ref != nil:
			p.add("%s: has both url and ref, give exactly one", at)
		case l.URL == "" && l.Ref == nil:
			p.add("%s: needs a url or a ref", at)
		case l.Ref != nil:
			if !RefKinds[l.Ref.Kind] {
				p.add("%s.ref.kind: %q is not one of commit, alert, incident, conversation, other", at, l.Ref.Kind)
			}
			if strings.TrimSpace(l.Ref.ID) == "" {
				p.add("%s.ref.id: required", at)
			}
			p.long(at+".ref.id", l.Ref.ID, maxRefID)
		default:
			p.url(at, l.URL)
		}
	}
}

// Validate checks a component as beacon serves it: class required, reporting allowed.
func (c *Component) Validate() error { return c.validate(true) }

// ValidateProducer checks a component as a producer writes it (beacon put): class optional
// (service), reporting beacon's alone.
func (c *Component) ValidateProducer() error { return c.validate(false) }

func (c *Component) validate(served bool) error {
	var p problems
	switch {
	case c.Class == "" && served:
		p.add("class: required (service, advisory or list)")
	case c.Class != "" && !validClass(c.Class):
		p.add("class: %q is not one of service, advisory, list", c.Class)
	}
	if r := c.Reporting; r != nil {
		if !served {
			p.add("reporting: is beacon's alone, a producer doesn't write it")
		} else {
			if strings.TrimSpace(r.Error) == "" {
				p.add("reporting.error: required")
			}
			p.long("reporting.error", r.Error, 500)
			if r.Since.IsZero() {
				p.add("reporting.since: required")
			}
		}
	}
	p.items(c.Items)
	// A producer may leave out id and updated_at: beacon put fills them in.
	if !ValidName(c.ID) && (served || c.ID != "") {
		p.add("id: %q is not a valid id ([a-z0-9][a-z0-9._-]*, at most 64)", c.ID)
	}
	p.long("title", c.Title, 100)
	p.long("group", c.Group, 50)
	p.long("description", c.Description, 300)
	if c.UpdatedAt.IsZero() && served {
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
		p.add("kind: must be reboot, deploy, config, note, service or change")
	}
	if e.Component != "" && !ValidName(e.Component) {
		p.add("component: %q is not a valid id", e.Component)
	}
	if strings.TrimSpace(e.Title) == "" {
		p.add("title: required")
	}
	p.long("title", e.Title, 200)
	p.long("detail", e.Detail, 2000)
	p.actor(e.Actor)
	p.eventLinks(e.Links)
	return p.err()
}

// Decode reads exactly one JSON object into v; unknown fields are an error.
func Decode(data []byte, v any) error { return decode(data, v, MaxBytes) }

func decode(data []byte, v any, max int) error {
	if len(data) > max {
		return fmt.Errorf("%d bytes, at most %d", len(data), max)
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

// ParseComponent decodes and validates a component document as served (a file in the
// components folder).
func ParseComponent(data []byte) (*Component, error) {
	var c Component
	if err := Decode(data, &c); err != nil {
		return nil, err
	}
	return &c, c.Validate()
}

// ParseProducerComponent decodes and validates a component as a producer writes it.
func ParseProducerComponent(data []byte) (*Component, error) {
	var c Component
	if err := Decode(data, &c); err != nil {
		return nil, err
	}
	return &c, c.ValidateProducer()
}

// ParseRun decodes and validates a run document (a log entry).
func ParseRun(data []byte) (*Run, error) {
	var r Run
	if err := Decode(data, &r); err != nil {
		return nil, err
	}
	return &r, r.Validate()
}

// Adapter is what a state adapter prints: the component's state, without what the
// definition (id, class, title…) or beacon (reporting) owns.
type Adapter struct {
	Status  Status   `json:"status,omitempty"`
	Summary string   `json:"summary,omitempty"`
	Gauges  []Metric `json:"gauges,omitempty"`
	Checks  []Check  `json:"checks,omitempty"`
	Links   []Link   `json:"links,omitempty"`
	Items   []Item   `json:"items,omitempty"`
}

// Validate checks an adapter's output on its own, as the component it would become.
func (a *Adapter) Validate() error {
	return (&Component{ID: "adapter", UpdatedAt: time.Unix(1, 0), Status: a.Status, Summary: a.Summary,
		Gauges: a.Gauges, Checks: a.Checks, Links: a.Links, Items: a.Items}).ValidateProducer()
}

// ParseAdapter decodes and validates a state adapter's output.
func ParseAdapter(data []byte) (*Adapter, error) {
	var a Adapter
	if err := Decode(data, &a); err != nil {
		return nil, err
	}
	return &a, a.Validate()
}

// Box is the box's facts in the envelope.
type Box struct {
	OS     string     `json:"os,omitempty"`
	Kernel string     `json:"kernel,omitempty"`
	Arch   string     `json:"arch,omitempty"`
	BootAt *time.Time `json:"boot_at,omitempty"`
}

// Bounds are the log's first and last seq.
type Bounds struct {
	First int64 `json:"first_seq"`
	Last  int64 `json:"last_seq"`
}

// Envelope is what GET /v3/state answers.
type Envelope struct {
	Schema      int          `json:"schema"`
	Source      string       `json:"source"`
	Beacon      string       `json:"beacon"`
	GeneratedAt time.Time    `json:"generated_at"`
	Box         Box          `json:"box"`
	Log         *Bounds      `json:"log"`
	Components  []*Component `json:"components"`
	Invalid     []Invalid    `json:"invalid"`
}

// MaxEnvelopeBytes bounds what ParseEnvelope reads (a box's whole state).
const MaxEnvelopeBytes = 64 << 20

func (e *Envelope) Validate() error {
	var p problems
	if e.Schema != Schema {
		p.add("schema: %d, this reads only %d", e.Schema, Schema)
	}
	if strings.TrimSpace(e.Source) == "" {
		p.add("source: required")
	}
	if strings.TrimSpace(e.Beacon) == "" {
		p.add("beacon: required")
	}
	if e.GeneratedAt.IsZero() {
		p.add("generated_at: required")
	}
	if e.Log == nil {
		p.add("log: required")
	} else if e.Log.First < 0 || e.Log.Last < e.Log.First {
		p.add("log: first_seq %d, last_seq %d", e.Log.First, e.Log.Last)
	}
	if e.Components == nil {
		p.add("components: required")
	}
	if e.Invalid == nil {
		p.add("invalid: required")
	}
	seen := map[string]bool{}
	for i, c := range e.Components {
		if c == nil {
			p.add("components[%d]: null", i)
			continue
		}
		if err := c.Validate(); err != nil {
			p.add("components[%d] (%s): %v", i, c.ID, err)
		}
		if seen[c.ID] {
			p.add("components[%d]: id %q appears twice", i, c.ID)
		}
		seen[c.ID] = true
	}
	for i, inv := range e.Invalid {
		if inv.File == "" || inv.Error == "" {
			p.add("invalid[%d]: file and error are required", i)
		}
	}
	return p.err()
}

// ParseEnvelope decodes and validates a /v3/state answer.
func ParseEnvelope(data []byte) (*Envelope, error) {
	var e Envelope
	if err := decode(data, &e, MaxEnvelopeBytes); err != nil {
		return nil, err
	}
	return &e, e.Validate()
}

// WriteComponent validates c (class defaults to service) and writes dir/<id>.json atomically.
func WriteComponent(dir string, c *Component) error {
	if c.Class == "" {
		c.Class = Service
	}
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
