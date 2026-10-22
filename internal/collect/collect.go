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
	ID          string `json:"id"`
	Title       string `json:"title,omitempty"`
	Group       string `json:"group,omitempty"`
	Description string `json:"description,omitempty"`
	ExpectEvery string `json:"expect_every,omitempty"` // default 10m (collect runs every minute)
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
	State string `json:"state,omitempty"` // command printing {status, summary, gauges, checks, links}
	Run   string `json:"run,omitempty"`   // command printing {summary, stats, artifacts, error} for a finished run
}

func (d *Definition) validate(file string) error {
	if !spec.ValidName(d.ID) || d.ID+".json" != file {
		return fmt.Errorf("id %q must be a valid id and match the file name", d.ID)
	}
	if d.Systemd != nil && len(d.Systemd.Units) == 0 {
		return errors.New("systemd.units: required")
	}
	if d.Systemd != nil && d.Systemd.Trigger != "" && !spec.Triggers[d.Systemd.Trigger] {
		return fmt.Errorf("systemd.trigger: %q is not a trigger", d.Systemd.Trigger)
	}
	if d.Schedule != nil && d.Schedule.Interval != "" {
		if _, err := spec.ParseDuration(d.Schedule.Interval); err != nil {
			return fmt.Errorf("schedule.interval: %w", err)
		}
	}
	return nil
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
		var d Definition
		if err == nil {
			err = spec.Decode(data, &d)
		}
		if err == nil {
			err = d.validate(e.Name())
		}
		if err != nil {
			invalid = append(invalid, spec.Invalid{File: "components.d/" + e.Name(), Error: err.Error()})
			continue
		}
		defs[d.ID] = &d
	}
	return defs, invalid
}

func NewID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Result is what one collect did, for the command's output.
type Result struct {
	Lines  []string
	Failed int
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

	// The host.
	if cfg.HostFeed && host.Supported {
		c, err := host.Component(now)
		if err == nil {
			err = spec.WriteComponent(cfg.Components, c)
		}
		if err != nil {
			res.fail("host: %v", err)
		} else {
			res.ok("host")
		}
	}

	defs, _ := Definitions(cfg.Definitions)
	var timers []systemd.Timer
	needTimers := false
	for _, d := range defs {
		needTimers = needTimers || (d.Systemd != nil && len(d.Systemd.Timers) > 0)
	}
	if needTimers && systemd.Supported {
		timers, _ = systemd.Timers()
	}

	// Runs of `beacon exec` whose process is gone (killed with the box, OOM, …).
	st, _ := statefile.Read(cfg.State)
	for id, m := range st.Running {
		if m.PID > 0 && syscall.Kill(m.PID, 0) != nil {
			run := &spec.Run{Type: "run", Component: m.Component, ID: id, StartedAt: m.StartedAt, EndedAt: now, Outcome: spec.Failure, Trigger: m.Trigger,
				Error: "the process ended without beacon recording it (killed, or the box went down)"}
			if err := FinishRun(ctx, cfg, defs[m.Component], run); err != nil {
				res.fail("%s: %v", m.Component, err)
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

type stateOutput struct {
	Status  spec.Status   `json:"status,omitempty"`
	Summary string        `json:"summary,omitempty"`
	Gauges  []spec.Metric `json:"gauges,omitempty"`
	Checks  []spec.Check  `json:"checks,omitempty"`
	Links   []spec.Link   `json:"links,omitempty"`
}

func component(ctx context.Context, cfg Config, d *Definition, timers []systemd.Timer, now time.Time, res *Result) error {
	c := &spec.Component{ID: d.ID, Title: d.Title, Group: d.Group, Description: d.Description, UpdatedAt: now, ExpectEvery: d.ExpectEvery}
	if c.ExpectEvery == "" {
		c.ExpectEvery = "10m"
	}
	var notes []string

	// systemd: new invocations become runs; one in progress becomes `running`.
	if d.Systemd != nil {
		if !systemd.Supported {
			return errors.New("systemd units listed, but systemd isn't available here")
		}
		for _, name := range d.Systemd.Units {
			u, err := systemd.Show(name)
			if err != nil {
				notes = append(notes, err.Error())
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
		if d.Systemd != nil {
			for _, t := range timers {
				for _, want := range d.Systemd.Timers {
					if t.Unit == want && !t.Next.IsZero() && (s.NextAt == nil || t.Next.Before(*s.NextAt)) {
						next := t.Next
						s.NextAt = &next
					}
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

	// State adapter: gauges, checks, summary, status, links.
	if d.State != "" {
		out, err := runCommand(ctx, d.State, []string{"BEACON_COMPONENT=" + d.ID}, cfg.Timeout)
		var so stateOutput
		if err == nil {
			err = spec.Decode(out, &so)
		}
		if err != nil {
			c.Status, c.Summary = spec.Fail, truncate("state adapter failed: "+err.Error(), 500)
		} else {
			c.Status, c.Summary, c.Gauges, c.Checks, c.Links = so.Status, so.Summary, so.Gauges, so.Checks, so.Links
		}
	}
	if len(notes) > 0 && c.Status != spec.Fail {
		c.Checks = append(c.Checks, spec.Check{Name: "beacon", Status: spec.Warn, Detail: truncate(strings.Join(notes, "; "), 500)})
	}
	if err := spec.WriteComponent(cfg.Components, c); err != nil {
		return err
	}
	res.ok("%s", d.ID)
	return nil
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
		out, err := runCommand(ctx, d.Run, env, cfg.Timeout)
		var extra struct {
			Summary   string        `json:"summary,omitempty"`
			Stats     []spec.Metric `json:"stats,omitempty"`
			Artifacts []spec.Link   `json:"artifacts,omitempty"`
			Error     string        `json:"error,omitempty"`
		}
		if err == nil {
			err = spec.Decode(out, &extra)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: run adapter: %v\n", run.Component, err)
		} else {
			if extra.Summary != "" {
				run.Summary = truncate(extra.Summary, 500)
			}
			run.Stats, run.Artifacts = extra.Stats, extra.Artifacts
			if extra.Error != "" {
				run.Error = truncate(extra.Error, 2000)
			}
		}
	}
	if _, err := (logbook.Log{Path: cfg.Log}).Append(run); err != nil {
		return err
	}
	return statefile.Update(cfg.State, func(s *statefile.State) error {
		delete(s.Running, run.ID)
		if s.LastStart == nil {
			s.LastStart = map[string]time.Time{}
		}
		if run.StartedAt.After(s.LastStart[run.Component]) {
			s.LastStart[run.Component] = run.StartedAt
		}
		return nil
	})
}

// runCommand runs an adapter in its own process group (all of it killed on timeout) and
// returns its stdout; stderr's last line explains a failure.
func runCommand(ctx context.Context, path string, env []string, timeout time.Duration) ([]byte, error) {
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
