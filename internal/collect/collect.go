// Package collect is `beacon collect` (every minute) and the shared run bookkeeping that
// `beacon exec` uses. It turns the box into components, runs and events (SPEC.md):
//
//   - reboots: a new boot id becomes a "reboot" event
//   - the built-in host component
//   - each definition in components.d: runs of its systemd units (every new invocation,
//     with outcome, exit code and the journal's last lines when it failed), a run in
//     progress, the schedule from its timers, gauges/checks from its state adapter, and
//     stats for each finished run from its run adapter
//   - `beacon exec` runs whose process vanished are logged as failed
//   - the log is pruned hourly
package collect

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/eemax/beacon/internal/host"
	"github.com/eemax/beacon/internal/logbook"
	"github.com/eemax/beacon/internal/probe"
	"github.com/eemax/beacon/internal/spec"
	"github.com/eemax/beacon/internal/statefile"
	"github.com/eemax/beacon/internal/systemd"
)

type Config struct {
	Components  string // folder of component state files (served)
	Definitions string // folder of component definitions (components.d)
	Log         string // the log file
	State       string // beacon's own state file
	HostFeed    bool   // write the built-in host component
	Timeout     time.Duration
	Now         func() time.Time
}

// Definition says what a component is and where its data comes from
// (/etc/beacon/components.d/<id>.json).
type Definition struct {
	ID          string     `json:"id"`
	Class       spec.Class `json:"class,omitempty"` // service (default), advisory or list
	Title       string     `json:"title,omitempty"`
	Group       string     `json:"group,omitempty"`
	Description string     `json:"description,omitempty"`
	ExpectEvery string     `json:"expect_every,omitempty"` // default 10m (collect runs every minute)
	Systemd     *struct {
		Units   []string `json:"units"`
		Timers  []string `json:"timers,omitempty"`
		Trigger string   `json:"trigger,omitempty"` // for runs no timer started: manual (default) or dependency
	} `json:"systemd,omitempty"`
	Schedule *struct {
		Every    string `json:"every,omitempty"`    // human description
		Grace    string `json:"grace,omitempty"`    // default 1h
		Interval string `json:"interval,omitempty"` // without timers: next run = last start + interval
	} `json:"schedule,omitempty"`
	Probes []probe.Probe `json:"probes,omitempty"` // HTTP(S)/TCP checks, as a client would see the service
	State  string        `json:"state,omitempty"`  // command printing {status, summary, gauges, checks, links, items}
	// How often to run the state adapter (default: every collect). Between runs its last
	// result is reused, so a slow or rate-limited adapter needn't run every minute.
	StateEvery string `json:"state_every,omitempty"`
	Run        string `json:"run,omitempty"` // command printing {summary, stats, artifacts, error} for a finished run
}

// ParseDefinition decodes and validates a definition document.
func ParseDefinition(data []byte) (*Definition, error) {
	var d Definition
	if err := spec.Decode(data, &d); err != nil {
		return nil, err
	}
	return &d, d.validate()
}

func (d *Definition) validate() error {
	if !spec.ValidName(d.ID) {
		return fmt.Errorf("id %q must be a valid id", d.ID)
	}
	if d.Class != "" && d.Class != spec.Service && d.Class != spec.Advisory && d.Class != spec.List {
		return fmt.Errorf("class: %q is not one of service, advisory, list", d.Class)
	}
	if d.ID == "host" {
		return errors.New(`id "host" is beacon's built-in component`)
	}
	if d.Systemd != nil && len(d.Systemd.Units) == 0 {
		return errors.New("systemd.units: required")
	}
	if len(d.Probes) > probe.MaxCount {
		return fmt.Errorf("probes: %d, at most %d", len(d.Probes), probe.MaxCount)
	}
	names := map[string]bool{}
	for i, p := range d.Probes {
		if err := p.Validate(); err != nil {
			return fmt.Errorf("probes[%d]: %w", i, err)
		}
		if names[p.Label()] {
			return fmt.Errorf("probes[%d]: %q twice (set name)", i, p.Label())
		}
		names[p.Label()] = true
	}
	if d.Systemd != nil && d.Systemd.Trigger != "" && !spec.Triggers[d.Systemd.Trigger] {
		return fmt.Errorf("systemd.trigger: %q is not a trigger", d.Systemd.Trigger)
	}
	if d.Schedule != nil && d.Schedule.Interval != "" {
		if _, err := spec.ParseDuration(d.Schedule.Interval); err != nil {
			return fmt.Errorf("schedule.interval: %w", err)
		}
	}
	if d.StateEvery != "" {
		if _, err := spec.ParseDuration(d.StateEvery); err != nil {
			return fmt.Errorf("state_every: %w", err)
		}
		if d.State == "" {
			return errors.New("state_every: there's no state adapter")
		}
	}
	// What the definition puts into the component (title, expect_every, schedule) must be
	// valid there, or every collect would fail to write it.
	c := spec.Component{ID: d.ID, Class: d.class(), Title: d.Title, Group: d.Group, Description: d.Description, UpdatedAt: time.Unix(1, 0), ExpectEvery: d.ExpectEvery}
	if d.Schedule != nil {
		c.Schedule = &spec.Schedule{Grace: d.Schedule.Grace, Every: d.Schedule.Every}
	}
	return c.Validate()
}

// class is the definition's class, service when unset.
func (d *Definition) class() spec.Class {
	if d.Class == "" {
		return spec.Service
	}
	return d.Class
}

// Definitions reads components.d; a broken file is reported, not fatal.
func Definitions(dir string) (map[string]*Definition, []spec.Invalid) {
	defs := map[string]*Definition{}
	var invalid []spec.Invalid
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") || !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		var d *Definition
		if err == nil {
			d, err = ParseDefinition(data)
		}
		if err == nil && d.ID+".json" != e.Name() {
			err = fmt.Errorf("id %q must match the file name", d.ID)
		}
		if err != nil {
			invalid = append(invalid, spec.Invalid{File: "components.d/" + e.Name(), Error: err.Error()})
			continue
		}
		defs[d.ID] = d
	}
	return defs, invalid
}

func NewID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Result is what one collect did, for the command's output: Lines are what changed (runs,
// events, removals, failures), Updated the components written (quiet unless asked: collect
// runs every minute and the journal shouldn't fill with "nothing happened").
type Result struct {
	Lines   []string
	Updated []string
	Failed  int
}

func (r *Result) ok(format string, args ...any) {
	r.Lines = append(r.Lines, fmt.Sprintf(format, args...))
}
func (r *Result) fail(format string, args ...any) { r.Failed++; r.ok("FAILED "+format, args...) }

func Run(ctx context.Context, cfg Config) (*Result, error) {
	now := cfg.Now().UTC().Truncate(time.Second)
	res := &Result{}
	log := logbook.Log{Path: cfg.Log}
	for _, dir := range []string{cfg.Components, filepath.Dir(cfg.Log), filepath.Dir(cfg.State)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}

	// Reboots.
	facts := host.ReadFacts()
	var reboot *spec.Event
	statefile.Update(cfg.State, func(s *statefile.State) error {
		if s.BootID != "" && facts.BootID != "" && s.BootID != facts.BootID {
			detail := "kernel " + facts.Kernel
			if s.Kernel != "" && s.Kernel != facts.Kernel {
				detail = "kernel " + s.Kernel + " → " + facts.Kernel
			}
			at := facts.BootAt
			if at.IsZero() {
				at = now
			}
			reboot = &spec.Event{Type: "event", ID: NewID(), At: at, Kind: "reboot", Title: "Rebooted", Detail: detail}
		}
		if facts.BootID != "" {
			s.BootID, s.Kernel = facts.BootID, facts.Kernel
		}
		return nil
	})
	if reboot != nil {
		if _, err := log.Append(reboot); err != nil {
			res.fail("reboot event: %v", err)
		} else {
			res.ok("reboot: %s", reboot.Detail)
		}
	}

	defs, _ := Definitions(cfg.Definitions)

	// The host. Units a definition names are left to their component.
	if cfg.HostFeed && host.Supported {
		owned := map[string]bool{}
		for _, d := range defs {
			if d.Systemd != nil {
				for _, u := range append(slices.Clone(d.Systemd.Units), d.Systemd.Timers...) {
					owned[u] = true
				}
			}
		}
		st, _ := statefile.Read(cfg.State)
		c, memo, err := host.Component(now, host.Input{BootID: facts.BootID, Memo: st.Host, Owned: owned})
		if err == nil {
			err = spec.WriteComponent(cfg.Components, c)
		}
		if err != nil {
			res.fail("host: %v", err)
		} else {
			statefile.Update(cfg.State, func(s *statefile.State) error { s.Host = memo; return nil })
			res.Updated = append(res.Updated, "host")
		}
	}

	var timers []systemd.Timer
	needTimers := false
	for _, d := range defs {
		needTimers = needTimers || (d.Systemd != nil && len(d.Systemd.Timers) > 0)
	}
	if needTimers && systemd.Supported {
		timers, _ = systemd.Timers()
	}

	// Runs of `beacon exec` whose process is gone (killed with the box, OOM, …). A marker
	// from before this boot is gone even if its pid has been reused since.
	st, _ := statefile.Read(cfg.State)
	for id, m := range st.Running {
		beforeBoot := !facts.BootAt.IsZero() && m.StartedAt.Before(facts.BootAt.Add(-time.Minute))
		if beforeBoot || (m.PID > 0 && syscall.Kill(m.PID, 0) != nil) {
			run := &spec.Run{Type: "run", Component: m.Component, ID: id, StartedAt: m.StartedAt, EndedAt: now, Outcome: spec.Failure, Trigger: m.Trigger,
				Error: "the process ended without beacon recording it (killed, or the box went down)"}
			if err := FinishRun(ctx, cfg, defs[m.Component], run); err != nil {
				res.fail("%s: %v", m.Component, err)
			} else {
				res.ok("%s: run %s (failure: its process vanished)", m.Component, id)
			}
		}
	}

	defined := make([]string, 0, len(defs))
	for id := range defs {
		defined = append(defined, id)
	}
	sort.Strings(defined)

	// Components of definitions removed since the last collect go, with their history of
	// starts (or they'd come back as exec-only components below).
	statefile.Update(cfg.State, func(s *statefile.State) error {
		for _, old := range s.Defined {
			if defs[old] == nil {
				os.Remove(filepath.Join(cfg.Components, old+".json"))
				delete(s.LastStart, old)
				res.ok("%s: definition removed, component removed", old)
			}
		}
		s.Defined = defined
		return nil
	})

	// Components that only have `beacon exec`/`beacon run` runs need no definition: they
	// get a plain one while they've run within the log's 90 days, or run now.
	st, _ = statefile.Read(cfg.State)
	implicit := map[string]bool{}
	add := func(id string) {
		if defs[id] == nil && id != "host" && spec.ValidName(id) {
			implicit[id] = true
		}
	}
	for id, last := range st.LastStart {
		if now.Sub(last) < 90*24*time.Hour {
			add(id)
		}
	}
	for _, m := range st.Running {
		add(m.Component)
	}
	all := maps.Clone(defs)
	if all == nil {
		all = map[string]*Definition{}
	}
	for id := range implicit {
		all[id] = &Definition{ID: id}
	}
	ids := slices.Sorted(maps.Keys(all))
	for _, id := range ids {
		if err := component(ctx, cfg, all[id], timers, now, res); err != nil {
			res.fail("%s: %v", id, err)
		}
	}

	// Exec-only components that haven't run for 90 days go, and the log is pruned hourly.
	statefile.Update(cfg.State, func(s *statefile.State) error {
		for _, old := range s.Implicit {
			if all[old] == nil {
				os.Remove(filepath.Join(cfg.Components, old+".json"))
				res.ok("%s: no runs for 90 days, component removed", old)
			}
		}
		s.Implicit = slices.Sorted(maps.Keys(implicit))
		if now.Sub(s.PrunedAt) >= time.Hour {
			if n, err := log.Prune(now.AddDate(0, 0, -90), 20000); err != nil {
				res.fail("prune: %v", err)
			} else if n > 0 {
				res.ok("pruned %d log entries", n)
			}
			for id, last := range s.LastStart {
				if now.Sub(last) >= 90*24*time.Hour {
					delete(s.LastStart, id)
				}
			}
			s.PrunedAt = now
		}
		return nil
	})
	return res, nil
}

func component(ctx context.Context, cfg Config, d *Definition, timers []systemd.Timer, now time.Time, res *Result) error {
	c := &spec.Component{ID: d.ID, Class: d.class(), Title: d.Title, Group: d.Group, Description: d.Description, UpdatedAt: now, ExpectEvery: d.ExpectEvery}
	if c.ExpectEvery == "" {
		c.ExpectEvery = "10m"
	}
	var notes []string
	var services []systemd.Unit
	shown := map[string]systemd.Unit{} // the units looked at, for the timers below

	// systemd: a job's new invocations become runs, one in progress becomes `running`;
	// services are looked at below.
	if d.Systemd != nil {
		if !systemd.Supported {
			notes = append(notes, "systemd units listed, but systemd isn't available here")
		}
		for _, name := range d.Systemd.Units {
			if !systemd.Supported {
				break
			}
			u, err := systemd.Show(name)
			if err != nil {
				notes = append(notes, err.Error())
				continue
			}
			shown[name] = u
			if !u.Job() {
				services = append(services, u)
				continue
			}
			if u.Running() && !u.Start.IsZero() {
				c.Running = &spec.Running{ID: u.InvocationID, StartedAt: u.Start, Trigger: trigger(d, u, timers)}
				continue
			}
			if u.InvocationID == "" || u.Start.IsZero() {
				continue
			}
			st, _ := statefile.Read(cfg.State)
			if st.Seen[name] == u.InvocationID {
				continue
			}
			run := runFromUnit(d, u, timers)
			if err := FinishRun(ctx, cfg, d, run); err != nil {
				notes = append(notes, err.Error())
				continue
			}
			statefile.Update(cfg.State, func(s *statefile.State) error { s.Seen[name] = u.InvocationID; return nil })
			res.ok("%s: run %s (%s, %s)", d.ID, run.ID, run.Outcome, run.EndedAt.Sub(run.StartedAt))
		}
	}
	svcSummary := serviceState(cfg, d, services, now, c, res)

	// `beacon exec` runs in progress.
	st, _ := statefile.Read(cfg.State)
	for id, m := range st.Running {
		if m.Component == d.ID && c.Running == nil {
			c.Running = &spec.Running{ID: id, StartedAt: m.StartedAt, Trigger: m.Trigger}
		}
	}

	// Schedule: the timers' next elapse, or last start + interval.
	if d.Schedule != nil || (d.Systemd != nil && len(d.Systemd.Timers) > 0) {
		s := &spec.Schedule{Grace: "1h"}
		if d.Schedule != nil {
			s.Every = d.Schedule.Every
			if d.Schedule.Grace != "" {
				s.Grace = d.Schedule.Grace
			}
		}
		if d.Systemd != nil && systemd.Supported {
			running := func(unit string) bool {
				u, ok := shown[unit]
				if !ok {
					var err error
					if u, err = systemd.Show(unit); err != nil {
						return false
					}
					shown[unit] = u
				}
				return u.Running()
			}
			for _, want := range d.Systemd.Timers {
				next, scheduled := timerNext(want, timers, running)
				if !next.IsZero() && (s.NextAt == nil || next.Before(*s.NextAt)) {
					s.NextAt = &next
				}
				// Without this, a stopped or disabled timer would just make the schedule
				// disappear, and with it the hub's "missed".
				if !scheduled {
					notes = append(notes, want+" isn't scheduled (stopped, disabled or missing): no next run")
				}
			}
		}
		if s.NextAt == nil && d.Schedule != nil && d.Schedule.Interval != "" {
			if last, ok := st.LastStart[d.ID]; ok {
				iv, _ := spec.ParseDuration(d.Schedule.Interval)
				next := last.Add(iv)
				s.NextAt = &next
			}
		}
		c.Schedule = s
	}

	// State adapter: gauges, checks, summary, status, links, items. When it fails, times
	// out or prints an invalid document, the component keeps its last good output with
	// `reporting` set: a broken report is never the app being down.
	if d.State != "" {
		so, rep := adapterState(ctx, cfg, d, now)
		c.Status, c.Summary, c.Links, c.Items, c.Reporting = so.Status, so.Summary, so.Links, so.Items, rep
		c.Gauges, c.Checks = append(so.Gauges, c.Gauges...), append(so.Checks, c.Checks...) // the adapter's first
	}
	// Probes: rows of their own (probe=…), after the adapter's, so they're never lost to it.
	for i, p := range d.Probes {
		g, k := probe.Run(ctx, p, now)
		for j := range g {
			g[j].Primary = g[j].Primary && i == 0 // the first probe's numbers are the headline
			if i == 0 && g[j].Key == "latency" {
				g[j].Primary = true
			}
		}
		c.Gauges, c.Checks = append(c.Gauges, g...), append(c.Checks, k...)
	}
	if d.State == "" && c.Summary == "" {
		c.Summary = svcSummary
	}
	if msg := st.RunAdapter[d.ID]; msg != "" {
		notes = append(notes, "last run logged without its stats: "+msg)
	}
	if len(notes) > 0 && len(c.Checks) < 50 {
		c.Checks = append(c.Checks, spec.Check{Name: "beacon", Status: spec.Warn, Detail: truncate(strings.Join(notes, "; "), 500)})
	}
	fit(c)
	if err := spec.WriteComponent(cfg.Components, c); err != nil {
		return err
	}
	res.Updated = append(res.Updated, d.ID)
	return nil
}

// cached is a state adapter's last result (stdout, or why it failed), for state_every.
type cached struct {
	At    time.Time       `json:"at"`
	Out   json.RawMessage `json:"out,omitempty"`
	Error string          `json:"error,omitempty"`
}

// lastGood is a component's last good state adapter output, kept beside beacon's state
// file (good/<id>.json): what the component shows while the adapter fails. At is zero
// until there's one; FailingSince is when the current failure began, zero while it works.
type lastGood struct {
	At           time.Time       `json:"at,omitzero"`
	Out          json.RawMessage `json:"out,omitempty"`
	FailingSince time.Time       `json:"failing_since,omitzero"`
}

func goodPath(cfg Config, id string) string {
	return filepath.Join(filepath.Dir(cfg.State), "good", id+".json")
}

func readGood(cfg Config, id string) lastGood {
	var g lastGood
	if data, err := os.ReadFile(goodPath(cfg, id)); err == nil && json.Unmarshal(data, &g) == nil {
		return g
	}
	return lastGood{}
}

func writeGood(cfg Config, id string, g lastGood) {
	path := goodPath(cfg, id)
	if data, err := json.Marshal(g); err == nil && os.MkdirAll(filepath.Dir(path), 0o700) == nil {
		spec.WriteFileAtomic(path, data, 0o600)
	}
}

// adapterState is d's state adapter's part of its component, and the reporting that says
// the adapter is failing (nil while it works). A good output replaces the kept one and
// clears reporting at once; a failure (non-zero exit, timeout, not JSON, an invalid
// document, class or reporting included) keeps the last good output, or with none yet
// reads unknown, and never fail on that account.
func adapterState(ctx context.Context, cfg Config, d *Definition, now time.Time) (spec.Adapter, *spec.Reporting) {
	out, at, err := stateAdapter(ctx, cfg, d, now)
	var a *spec.Adapter
	if err == nil {
		a, err = spec.ParseAdapter(out)
	}
	good := readGood(cfg, d.ID)
	if err == nil {
		if !good.At.Equal(at) || !good.FailingSince.IsZero() {
			writeGood(cfg, d.ID, lastGood{At: at, Out: out})
		}
		return *a, nil
	}
	if good.FailingSince.IsZero() {
		good.FailingSince = now
		writeGood(cfg, d.ID, good)
	}
	msg := truncate("state adapter failed: "+oneLine(err.Error()), 500)
	rep := &spec.Reporting{Error: msg, Since: good.FailingSince}
	if !good.At.IsZero() {
		if kept, kerr := spec.ParseAdapter(good.Out); kerr == nil {
			last := good.At
			rep.LastGoodAt = &last
			return *kept, rep
		}
	}
	return spec.Adapter{Status: spec.Unknown}, rep // the reason is in reporting.error
}

// stateAdapter runs d's state adapter, or reuses its last result while that's younger than
// state_every (failures are kept too: a failing slow adapter isn't retried every minute).
// at is when the output was produced.
func stateAdapter(ctx context.Context, cfg Config, d *Definition, now time.Time) (out []byte, at time.Time, err error) {
	every, _ := spec.ParseDuration(d.StateEvery)
	path := filepath.Join(filepath.Dir(cfg.State), "cache", d.ID+".json")
	if every > 0 {
		var c cached
		if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &c) == nil && now.Sub(c.At) < every && !c.At.After(now) {
			if c.Error != "" {
				return nil, c.At, errors.New(c.Error)
			}
			return c.Out, c.At, nil
		}
	}
	out, err = RunCommand(ctx, d.State, []string{"BEACON_COMPONENT=" + d.ID}, cfg.Timeout)
	if every > 0 {
		c := cached{At: now}
		if err != nil {
			c.Error = err.Error()
		} else if json.Valid(out) {
			c.Out = out
		} else {
			c.Error = "not valid JSON"
		}
		if data, merr := json.Marshal(c); merr == nil && os.MkdirAll(filepath.Dir(path), 0o700) == nil {
			spec.WriteFileAtomic(path, data, 0o600)
		}
	}
	return out, now, err
}

// fit keeps a component within the SPEC limits once the adapter's, the services' and the
// probes' parts are together: a series that appears twice keeps its first value, and the
// lists are cut at their maximums (at most 6 headline numbers).
func fit(c *spec.Component) {
	seen := map[string]bool{}
	gauges := c.Gauges[:0:0]
	primary := 0
	for _, m := range c.Gauges {
		if seen[m.SeriesID()] || len(gauges) == 100 {
			continue
		}
		seen[m.SeriesID()] = true
		if m.Primary {
			if primary == 6 {
				m.Primary = false
			} else {
				primary++
			}
		}
		gauges = append(gauges, m)
	}
	c.Gauges = gauges
	if len(c.Checks) > 50 {
		c.Checks = c.Checks[:50]
	}
}

// serviceState adds a long-running unit's facts to the component (an "active" check, and
// uptime, restarts and memory, labelled unit=<name>) and logs a `service` event when
// systemd restarted it after a crash or it failed. Intended restarts (a deploy, systemctl
// restart) stay quiet: the deploy event says why. Returns a one-line summary.
func serviceState(cfg Config, d *Definition, units []systemd.Unit, now time.Time, c *spec.Component, res *Result) string {
	if len(units) == 0 {
		return ""
	}
	var events []*spec.Event
	var up, down []string
	statefile.Update(cfg.State, func(s *statefile.State) error {
		for _, u := range units {
			name := strings.TrimSuffix(u.Name, ".service")
			labels := spec.Labels{"unit": name}
			check := spec.Check{Name: "active", Status: spec.OK, Labels: labels}
			switch u.ActiveState {
			case "active", "reloading":
				up = append(up, name)
				if !u.ActiveSince.IsZero() {
					c.Gauges = append(c.Gauges, spec.Metric{Key: "uptime", Label: "Up for", Value: now.Sub(u.ActiveSince).Truncate(time.Second).Seconds(), Unit: "s", Labels: labels})
				}
			case "activating":
				check.Status, check.Detail = spec.Warn, "starting ("+u.SubState+")"
				down = append(down, name+" starting")
			case "failed":
				check.Status, check.Detail = spec.Fail, "failed (result: "+u.Result+")"
				down = append(down, name+" failed")
			default:
				check.Status, check.Detail = spec.Fail, u.ActiveState
				down = append(down, name+" "+u.ActiveState)
			}
			c.Checks = append(c.Checks, check)
			c.Gauges = append(c.Gauges, spec.Metric{Key: "restarts", Label: "Restarts", Value: float64(u.Restarts), Unit: "count", Labels: labels})
			if u.Memory >= 0 {
				c.Gauges = append(c.Gauges, spec.Metric{Key: "memory", Label: "Memory", Value: float64(u.Memory), Unit: "bytes", Labels: labels})
			}

			prev, known := s.Services[u.Name]
			switch {
			case !known:
			case u.Restarts > prev.Restarts && u.InvocationID != prev.Invocation:
				n := u.Restarts - prev.Restarts
				title := u.Name + " crashed and was restarted by systemd"
				if n > 1 {
					title = fmt.Sprintf("%s crashed and was restarted by systemd (%d times)", u.Name, n)
				}
				events = append(events, &spec.Event{Type: "event", ID: NewID(), At: now, Kind: "service", Component: d.ID, Title: title,
					Detail: truncate(lastLines(systemd.Journal(prev.Invocation, 15), 2000), 2000)})
			case u.ActiveState == "failed" && prev.State != "failed":
				events = append(events, &spec.Event{Type: "event", ID: NewID(), At: now, Kind: "service", Component: d.ID, Title: u.Name + " failed",
					Detail: truncate(lastLines(systemd.Journal(u.InvocationID, 15), 2000), 2000)})
			}
			s.Services[u.Name] = statefile.Service{Invocation: u.InvocationID, Restarts: u.Restarts, State: u.ActiveState}
		}
		return nil
	})
	for _, e := range events {
		if _, err := (logbook.Log{Path: cfg.Log}).Append(e); err != nil {
			res.fail("%s: service event: %v", d.ID, err)
		} else {
			res.ok("%s: %s", d.ID, e.Title)
		}
	}
	if len(down) > 0 {
		return strings.Join(down, ", ")
	}
	if len(up) == 1 {
		return up[0] + " is up"
	}
	return strings.Join(up, ", ") + " are up"
}

// timerNext is a timer's next elapse, and whether it's scheduled at all. While the unit it
// starts is running, systemd shows no next elapse (it computes one when the run ends):
// that timer is scheduled, just without a next_at for now.
func timerNext(want string, timers []systemd.Timer, running func(unit string) bool) (time.Time, bool) {
	var next time.Time
	scheduled := false
	for _, t := range timers {
		if t.Unit != want {
			continue
		}
		switch {
		case !t.Next.IsZero():
			scheduled = true
			if next.IsZero() || t.Next.Before(next) {
				next = t.Next
			}
		case t.Activates != "" && running(t.Activates):
			scheduled = true
		}
	}
	return next, scheduled
}

func trigger(d *Definition, u systemd.Unit, timers []systemd.Timer) string {
	for _, t := range timers {
		if t.Activates == u.Name && !t.Last.IsZero() && absDur(t.Last.Sub(u.Start)) <= 15*time.Second {
			return "timer"
		}
	}
	if d.Systemd != nil && d.Systemd.Trigger != "" {
		return d.Systemd.Trigger
	}
	return "manual"
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// Signals and exit codes that mean "stopped from outside", not "failed".
var cancelSignals = map[int]bool{1: true, 2: true, 15: true}
var cancelCodes = map[int]bool{129: true, 130: true, 143: true}

func runFromUnit(d *Definition, u systemd.Unit, timers []systemd.Timer) *spec.Run {
	end := u.Exit
	if end.IsZero() || end.Before(u.Start) {
		end = u.Start
	}
	run := &spec.Run{Type: "run", Component: d.ID, ID: u.InvocationID, StartedAt: u.Start, EndedAt: end, Unit: u.Name, Trigger: trigger(d, u, timers)}
	code := u.ExitStatus
	if u.ExitCode == 2 || u.ExitCode == 3 { // killed or dumped: status is the signal
		code = 128 + u.ExitStatus
	}
	run.ExitCode = &code
	switch {
	case u.Result == "success":
		run.Outcome = spec.Success
	case u.Result == "timeout": // systemd stopped it for running too long: it hung
		run.Outcome = spec.Failure
	case (u.ExitCode == 2 && cancelSignals[u.ExitStatus]) || (u.ExitCode == 1 && cancelCodes[u.ExitStatus]):
		run.Outcome = spec.Cancelled
	default:
		run.Outcome = spec.Failure
	}
	if run.Outcome != spec.Success {
		run.Error = truncate(lastLines(systemd.Journal(u.InvocationID, 30), 2000), 2000)
		if run.Error == "" {
			run.Error = "result: " + u.Result
		}
	}
	return run
}

// FinishRun adds the run adapter's stats to a finished run and logs it.
func FinishRun(ctx context.Context, cfg Config, d *Definition, run *spec.Run) error {
	adapterErr := "-" // "-": no adapter ran, keep what's recorded
	if d != nil && d.Run != "" {
		exit := ""
		if run.ExitCode != nil {
			exit = strconv.Itoa(*run.ExitCode)
		}
		env := []string{
			"BEACON_COMPONENT=" + run.Component, "BEACON_RUN_ID=" + run.ID, "BEACON_RUN_UNIT=" + run.Unit,
			"BEACON_RUN_OUTCOME=" + string(run.Outcome), "BEACON_RUN_EXIT_CODE=" + exit,
			"BEACON_RUN_STARTED_AT=" + run.StartedAt.Format(time.RFC3339), "BEACON_RUN_ENDED_AT=" + run.EndedAt.Format(time.RFC3339),
		}
		out, err := RunCommand(ctx, d.Run, env, cfg.Timeout)
		if err == nil {
			// Checked on a copy: a bad adapter document must never cost the run itself.
			var merged spec.Run
			if merged, err = mergeRunAdapter(out, *run); err == nil {
				*run = merged
			}
		}
		adapterErr = ""
		if err != nil {
			adapterErr = truncate(oneLine(err.Error()), 300)
			fmt.Fprintf(os.Stderr, "%s: run adapter: %s\n", run.Component, adapterErr)
		}
	}
	if _, err := (logbook.Log{Path: cfg.Log}).Append(run); err != nil {
		return err
	}
	return statefile.Update(cfg.State, func(s *statefile.State) error {
		delete(s.Running, run.ID)
		if adapterErr != "-" {
			if s.RunAdapter == nil {
				s.RunAdapter = map[string]string{}
			}
			if adapterErr == "" {
				delete(s.RunAdapter, run.Component)
			} else {
				s.RunAdapter[run.Component] = adapterErr
			}
		}
		if s.LastStart == nil {
			s.LastStart = map[string]time.Time{}
		}
		if run.StartedAt.After(s.LastStart[run.Component]) {
			s.LastStart[run.Component] = run.StartedAt
		}
		return nil
	})
}

// mergeRunAdapter adds a run adapter's document ({summary, stats, artifacts, error}) to
// run, and checks the result as a run.
func mergeRunAdapter(out []byte, run spec.Run) (spec.Run, error) {
	var extra struct {
		Summary   string        `json:"summary,omitempty"`
		Stats     []spec.Metric `json:"stats,omitempty"`
		Artifacts []spec.Link   `json:"artifacts,omitempty"`
		Error     string        `json:"error,omitempty"`
	}
	if err := spec.Decode(out, &extra); err != nil {
		return run, err
	}
	if extra.Summary != "" {
		run.Summary = truncate(extra.Summary, 500)
	}
	run.Stats, run.Artifacts = extra.Stats, extra.Artifacts
	if extra.Error != "" {
		run.Error = truncate(extra.Error, 2000)
	}
	return run, run.Validate()
}

// CheckDefinitions is `beacon check --definitions`: it runs each definition's state
// adapter (checked as an adapter) and run adapter (checked as a run: its document merged
// into a finished run of the component) once, with BEACON_COMPONENT set, and returns a
// line per definition, "ok <id>" or "BAD <id>: <why>", and how many are bad. only limits
// it to those ids (an id without a definition is bad). A dir it can't read, or one with
// no definition in it, is bad too. It needs no systemd and writes nothing.
func CheckDefinitions(ctx context.Context, dir string, only []string, timeout time.Duration) ([]string, int) {
	var lines []string
	bad := 0
	report := func(id string, problems []string) {
		if len(problems) == 0 {
			lines = append(lines, "ok "+id)
			return
		}
		bad++
		lines = append(lines, "BAD "+id+": "+strings.Join(problems, "; "))
	}
	want := map[string]bool{}
	for _, id := range only {
		want[id] = true
	}
	found := map[string]bool{}
	// The deploy gate: a directory that can't be read, or holds no definition to check,
	// is bad, never a pass that checked nothing.
	entries, err := os.ReadDir(dir)
	if err != nil {
		report(dir, []string{"definitions: " + oneLine(err.Error())})
		return lines, bad
	}
	for _, e := range entries {
		id, isJSON := strings.CutSuffix(e.Name(), ".json")
		if !isJSON || !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") || (len(want) > 0 && !want[id]) {
			continue
		}
		found[id] = true
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		var d *Definition
		if err == nil {
			d, err = ParseDefinition(data)
		}
		if err == nil && d.ID != id {
			err = fmt.Errorf("id %q must match the file name", d.ID)
		}
		if err != nil {
			report(id, []string{"definition: " + oneLine(err.Error())})
			continue
		}
		var problems []string
		if d.State != "" {
			out, err := RunCommand(ctx, d.State, []string{"BEACON_COMPONENT=" + d.ID}, timeout)
			if err == nil {
				_, err = spec.ParseAdapter(out)
			}
			if err != nil {
				problems = append(problems, "state adapter: "+truncate(oneLine(err.Error()), 300))
			}
		}
		if d.Run != "" {
			now := time.Now().UTC().Truncate(time.Second)
			code := 0
			env := []string{
				"BEACON_COMPONENT=" + d.ID, "BEACON_RUN_ID=check", "BEACON_RUN_UNIT=", "BEACON_RUN_OUTCOME=success", "BEACON_RUN_EXIT_CODE=0",
				"BEACON_RUN_STARTED_AT=" + now.Format(time.RFC3339), "BEACON_RUN_ENDED_AT=" + now.Format(time.RFC3339),
			}
			out, err := RunCommand(ctx, d.Run, env, timeout)
			if err == nil {
				_, err = mergeRunAdapter(out, spec.Run{Type: "run", Component: d.ID, ID: "check", StartedAt: now, EndedAt: now, Outcome: spec.Success, ExitCode: &code})
			}
			if err != nil {
				problems = append(problems, "run adapter: "+truncate(oneLine(err.Error()), 300))
			}
		}
		report(id, problems)
	}
	for _, id := range only {
		if !found[id] {
			found[id] = true
			report(id, []string{"no definition"})
		}
	}
	if len(found) == 0 {
		report(dir, []string{"no definitions"})
	}
	return lines, bad
}

// RunCommand runs an adapter in its own process group (all of it killed on timeout) and
// returns its stdout; stderr's last line explains a failure.
func RunCommand(ctx context.Context, path string, env []string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("%s: timed out after %s", filepath.Base(path), timeout)
		}
		if msg := lastLines(stderr.String(), 300); msg != "" {
			return nil, fmt.Errorf("%s: %v: %s", filepath.Base(path), err, msg)
		}
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return stdout.Bytes(), nil
}

// oneLine joins a multi-line error (errors.Join) into one line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// lastLines keeps the end of s, at most max bytes, starting at a line break.
func lastLines(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	s = s[len(s)-max:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
		s = s[i+1:]
	}
	return s
}

func truncate(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

// MarshalIndent is used by `beacon validate` and tests to show documents.
func MarshalIndent(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}
