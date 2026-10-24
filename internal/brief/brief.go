// Package brief writes the box's state as a short Markdown summary for people and AI agents
// on the box (beacon brief): what needs attention and why, what's running, what's due, and
// the log's recent runs and events. A component's status is derived the way a hub derives
// it (SPEC.md → How a hub presents it), so the brief and the hub agree.
package brief

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/eemax/beacon/internal/spec"
)

// State is a component's derived status: ok, unknown, warn, stale, missed or fail.
type State string

const (
	OK      State = "ok"
	Unknown State = "unknown"
	Warn    State = "warn"
	Stale   State = "stale"
	Missed  State = "missed"
	Fail    State = "fail"
)

var rank = map[State]int{OK: 0, Unknown: 1, Warn: 2, Stale: 3, Missed: 4, Fail: 5}

// slack is how late collect may be before that's a problem (it runs every minute).
const slack = 2 * time.Minute

const defaultExpect = 10 * time.Minute

// Derived is a component's status and why.
type Derived struct {
	State  State
	Reason string
}

// Derive is a component's status: the worst of its latest run failing, a missed run, a
// failing or warning check, stale state and the producer's own status.
func Derive(c *spec.Component, latest *spec.Run, now time.Time) Derived {
	var found []Derived
	every := defaultExpect
	if d, err := spec.ParseDuration(c.ExpectEvery); err == nil {
		every = d
	}
	if now.Sub(c.UpdatedAt) > every+slack {
		found = append(found, Derived{Stale, "no update since " + stamp(c.UpdatedAt)})
	}
	if latest != nil {
		switch latest.Outcome {
		case spec.Failure:
			why := latest.Summary
			if why == "" {
				why = lastLine(latest.Error)
			}
			if why == "" {
				why = "failed"
			}
			found = append(found, Derived{Fail, clip(fmt.Sprintf("last run (%s) failed: %s", stamp(latest.StartedAt), why), 300)})
		case spec.Cancelled:
			found = append(found, Derived{Warn, fmt.Sprintf("last run (%s) was stopped", stamp(latest.StartedAt))})
		}
	}
	if s := c.Schedule; s != nil && s.NextAt != nil && c.Running == nil {
		grace := time.Hour
		if d, err := spec.ParseDuration(s.Grace); err == nil {
			grace = d
		}
		ranSince := latest != nil && !latest.StartedAt.Before(s.NextAt.Add(-slack))
		if now.After(s.NextAt.Add(grace+slack)) && !ranSince {
			found = append(found, Derived{Missed, "missed the run due " + stamp(*s.NextAt)})
		}
	}
	if bad := badChecks(c.Checks); len(bad) > 0 {
		worst := bad[0]
		reason := checkText(worst)
		if len(bad) > 1 {
			reason += fmt.Sprintf(" (+%d more)", len(bad)-1)
		}
		found = append(found, Derived{State(worst.Status), reason})
	}
	if c.Status == spec.Fail || c.Status == spec.Warn {
		found = append(found, Derived{State(c.Status), cmp.Or(c.Summary, string(c.Status))})
	}
	if len(found) == 0 {
		if c.Status == spec.Unknown {
			return Derived{Unknown, cmp.Or(c.Summary, "the box can't tell")}
		}
		if latest != nil && latest.Outcome == spec.Success {
			return Derived{OK, cmp.Or(latest.Summary, c.Summary, "last run succeeded")}
		}
		return Derived{OK, c.Summary}
	}
	// Stable: of equally bad findings, the first one found wins (as in the hub).
	slices.SortStableFunc(found, func(a, b Derived) int { return rank[b.State] - rank[a.State] })
	return found[0]
}

// badChecks are the failing checks, then the warning ones, in their order.
func badChecks(cs []spec.Check) []spec.Check {
	var fail, warn []spec.Check
	for _, c := range cs {
		switch c.Status {
		case spec.Fail:
			fail = append(fail, c)
		case spec.Warn:
			warn = append(warn, c)
		}
	}
	return append(fail, warn...)
}

func checkText(c spec.Check) string {
	what := c.Name
	for _, k := range sortedKeys(c.Labels) {
		what += " · " + c.Labels[k]
	}
	if c.Detail != "" {
		what += ": " + c.Detail
	}
	return what
}

// Input is everything a brief is made of.
type Input struct {
	Source     string
	Components []*spec.Component
	Invalid    []spec.Invalid
	Log        []json.RawMessage // oldest first, as stored
	Now        time.Time
	Window     time.Duration // how far back "Recent" goes
}

type entry struct {
	run   *spec.Run
	event *spec.Event
}

func (e entry) when() time.Time {
	if e.run != nil {
		return e.run.EndedAt
	}
	return e.event.At
}

func parseLog(raw []json.RawMessage) []entry {
	var out []entry
	for _, line := range raw {
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &head) != nil {
			continue
		}
		switch head.Type {
		case "run":
			var r spec.Run
			if json.Unmarshal(line, &r) == nil {
				out = append(out, entry{run: &r})
			}
		case "event":
			var e spec.Event
			if json.Unmarshal(line, &e) == nil {
				out = append(out, entry{event: &e})
			}
		}
	}
	return out
}

const maxRecent = 40

// Write prints the brief.
func Write(w io.Writer, in Input) error {
	log := parseLog(in.Log)
	latest := map[string]*spec.Run{}
	for _, e := range log {
		if e.run != nil {
			latest[e.run.Component] = e.run
		}
	}
	type row struct {
		c *spec.Component
		d Derived
	}
	rows := make([]row, 0, len(in.Components))
	problems := 0
	for _, c := range in.Components {
		d := Derive(c, latest[c.ID], in.Now)
		if d.State != OK {
			problems++
		}
		rows = append(rows, row{c, d})
	}
	slices.SortStableFunc(rows, func(a, b row) int { return rank[b.d.State] - rank[a.d.State] })
	problems += len(in.Invalid)

	var b strings.Builder
	headline := "all components ok"
	switch {
	case len(rows) == 0:
		headline = "no components yet"
	case problems == 1:
		headline = "1 component needs attention"
	case problems > 1:
		headline = fmt.Sprintf("%d components need attention", problems)
	}
	fmt.Fprintf(&b, "# %s: %s\n\nbeacon brief at %s. Times are UTC. More: `beacon log`, the files in /var/lib/beacon/components/.\n", in.Source, headline, stamp(in.Now))

	if len(rows) > 0 {
		b.WriteString("\n## Components\n\n")
	}
	for _, r := range rows {
		c := r.c
		fmt.Fprintf(&b, "- %s **%s**", r.d.State, c.ID)
		if c.Title != "" && c.Title != c.ID {
			fmt.Fprintf(&b, " (%s)", c.Title)
		}
		if r.d.Reason != "" {
			fmt.Fprintf(&b, ": %s", oneLine(r.d.Reason))
		}
		b.WriteString("\n")
		if bad := badChecks(c.Checks); len(bad) > 1 {
			for _, k := range bad {
				fmt.Fprintf(&b, "  - %s %s\n", k.Status, oneLine(checkText(k)))
			}
		}
		if nums := headlines(c.Gauges); nums != "" {
			fmt.Fprintf(&b, "  - %s\n", nums)
		}
		if run := c.Running; run != nil {
			fmt.Fprintf(&b, "  - running since %s%s\n", stamp(run.StartedAt), trig(run.Trigger))
		}
		if s := c.Schedule; s != nil && (s.NextAt != nil || s.Every != "") {
			line := "  - schedule:"
			if s.Every != "" {
				line += " " + s.Every + ";"
			}
			if s.NextAt != nil {
				line += " next run " + stamp(*s.NextAt)
			}
			b.WriteString(strings.TrimSuffix(line, ";") + "\n")
		}
	}
	for _, inv := range in.Invalid {
		fmt.Fprintf(&b, "- fail **%s**: not a valid component: %s\n", inv.File, oneLine(clip(inv.Error, 300)))
	}

	since := in.Now.Add(-in.Window)
	var recent []entry
	quiet := map[string]int{} // successful runs not shown, per component
	lastOK := map[string]*spec.Run{}
	for _, e := range log {
		if e.when().Before(since) {
			continue
		}
		if e.run != nil && e.run.Outcome == spec.Success {
			if prev := lastOK[e.run.Component]; prev != nil {
				quiet[e.run.Component]++
			}
			lastOK[e.run.Component] = e.run
			continue
		}
		recent = append(recent, e)
	}
	// Successful runs: only the latest per component, with a count of the others.
	for _, r := range lastOK {
		recent = append(recent, entry{run: r})
	}
	slices.SortStableFunc(recent, func(a, b entry) int { return b.when().Compare(a.when()) })
	fmt.Fprintf(&b, "\n## Recent (since %s, newest first)\n\n", stamp(since))
	if len(recent) == 0 {
		b.WriteString("Nothing logged.\n")
	}
	for i, e := range recent {
		if i == maxRecent {
			fmt.Fprintf(&b, "- … %d more (`beacon log --limit 200`)\n", len(recent)-maxRecent)
			break
		}
		if r := e.run; r != nil {
			fmt.Fprintf(&b, "- %s run **%s** %s after %s%s", stamp(r.EndedAt), r.Component, r.Outcome, dur(r.EndedAt.Sub(r.StartedAt)), trig(r.Trigger))
			if r.ExitCode != nil && r.Outcome == spec.Failure {
				fmt.Fprintf(&b, ", exit %d", *r.ExitCode)
			}
			why := r.Summary
			if why == "" && r.Outcome == spec.Failure {
				why = lastLine(r.Error)
			}
			if why != "" {
				fmt.Fprintf(&b, ": %s", oneLine(clip(why, 300)))
			}
			if n := quiet[r.Component]; n > 0 && r.Outcome == spec.Success {
				fmt.Fprintf(&b, " (+%d earlier successful %s)", n, plural(n, "run", "runs"))
			}
			b.WriteString("\n")
			continue
		}
		ev := e.event
		fmt.Fprintf(&b, "- %s %s", stamp(ev.At), ev.Kind)
		if ev.Component != "" {
			fmt.Fprintf(&b, " **%s**", ev.Component)
		}
		fmt.Fprintf(&b, ": %s", oneLine(ev.Title))
		if ev.Detail != "" {
			fmt.Fprintf(&b, ": %s", clip(oneLine(ev.Detail), 200))
		}
		b.WriteString("\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// headlines are a component's headline numbers: the primary gauges (unlabelled first), or
// with none marked the first unlabelled ones; at most 4.
func headlines(gauges []spec.Metric) string {
	var pick []spec.Metric
	for _, m := range gauges {
		if m.Primary && len(m.Labels) == 0 {
			pick = append(pick, m)
		}
	}
	for _, m := range gauges {
		if m.Primary && len(m.Labels) > 0 {
			pick = append(pick, m)
		}
	}
	if len(pick) == 0 {
		for _, m := range gauges {
			if len(m.Labels) == 0 {
				pick = append(pick, m)
			}
		}
	}
	parts := make([]string, 0, 4)
	for _, m := range pick[:min(len(pick), 4)] {
		name := cmp.Or(m.Label, m.Key)
		if len(m.Labels) > 0 {
			vals := make([]string, 0, len(m.Labels))
			for _, k := range sortedKeys(m.Labels) {
				vals = append(vals, m.Labels[k])
			}
			name += " (" + strings.Join(vals, ", ") + ")"
		}
		parts = append(parts, name+" "+Format(m.Value, m.Unit))
	}
	return strings.Join(parts, " · ")
}

// Format writes a value in its unit (SPEC.md → Metric.unit).
func Format(v float64, unit string) string {
	switch unit {
	case "count", "":
		return number(v)
	case "percent":
		return number(v) + " %"
	case "bytes":
		return fmtBytes(v)
	case "s":
		return dur(time.Duration(v * float64(time.Second)))
	case "ms":
		return number(v) + " ms"
	}
	if len(unit) == 3 && strings.ToLower(unit) == unit && strings.Trim(unit, "abcdefghijklmnopqrstuvwxyz") == "" {
		code := strings.ToUpper(unit)
		if code == "USD" {
			return "$" + strconv.FormatFloat(v, 'f', 2, 64)
		}
		return strconv.FormatFloat(v, 'f', 2, 64) + " " + code
	}
	return number(v) + " " + unit
}

// number: integers as they are, others with at most 2 decimals (small ones to 2 digits).
func number(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return grouped(int64(v))
	}
	if math.Abs(v) < 0.01 {
		return strconv.FormatFloat(v, 'g', 2, 64)
	}
	s := strconv.FormatFloat(v, 'f', 2, 64)
	return strings.TrimRight(strings.TrimRight(s, "0"), ".")
}

// grouped writes an integer with thousands separators: 367,138.
func grouped(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}

func fmtBytes(v float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for math.Abs(v) >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return number(v) + " B"
	}
	return strconv.FormatFloat(v, 'f', 1, 64) + " " + units[i]
}

// dur is a duration in at most two units: 45s, 3m12s, 2h05m, 3d4h.
func dur(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
}

func stamp(t time.Time) string { return t.UTC().Truncate(time.Second).Format(time.RFC3339) }

func trig(t string) string {
	if t == "" {
		return ""
	}
	return " (" + t + ")"
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func sortedKeys(l spec.Labels) []string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
