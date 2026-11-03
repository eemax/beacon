// Package host is the box itself: the built-in "host" component (load, memory, swap,
// disk, uptime; on Linux failed units, OOM kills and a pending reboot) and the box's facts
// (OS, kernel, boot), on Linux and macOS. The checks' levels are SPEC.md's.
package host

import (
	"fmt"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/eemax/beacon/internal/spec"
	"github.com/eemax/beacon/internal/statefile"
)

// Input is what the host component needs besides the box itself.
type Input struct {
	BootID string
	Memo   statefile.Host  // what the last collect remembered
	Owned  map[string]bool // systemd units a component's definition watches
}

// OOMWindow is how long an OOM kill keeps the oom check warning; RebootNights how many
// midnights (UTC) a pending reboot may last before the reboot check warns.
const (
	OOMWindow    = 24 * time.Hour
	RebootNights = 3
)

// Facts describe the box (served as "box" in /v2/state). BootID changes on every boot.
type Facts struct {
	OS     string    `json:"os,omitempty"`
	Kernel string    `json:"kernel,omitempty"`
	Arch   string    `json:"arch,omitempty"`
	BootAt time.Time `json:"boot_at,omitempty"`
	BootID string    `json:"-"`
}

// sample is what each platform reads; Component turns it into gauges and checks.
type sample struct {
	load                          [3]float64
	memTotal, memAvail            float64 // bytes
	swapTotal, swapUsed           float64 // bytes
	diskTotal, diskUsed, diskFree float64 // bytes; used/free as a user sees them
	uptime                        float64 // seconds
	// Linux only; *Known says whether it could be read.
	rebootKnown, rebootPending bool
	rebootAt                   time.Time // the file's modification time
	rebootPkgs                 []string  // /var/run/reboot-required.pkgs
	oomKnown                   bool
	oomKills                   uint64 // /proc/vmstat oom_kill: since boot
	unitsKnown                 bool
	failedUnits                []string
}

// Component reads the host now; it returns what the next collect needs remembered.
func Component(now time.Time, in Input) (*spec.Component, statefile.Host, error) {
	s, err := read()
	if err != nil {
		return nil, in.Memo, err
	}
	c, memo := component(now, s, in, oomVictims)
	return c, memo, nil
}

func component(now time.Time, s sample, in Input, victims func(since time.Time) []string) (*spec.Component, statefile.Host) {
	now = now.UTC().Truncate(time.Second)
	pct := func(part, total float64) float64 {
		if total <= 0 {
			return 0
		}
		return float64(int(part/total*1000+0.5)) / 10
	}
	memUsed := pct(s.memTotal-s.memAvail, s.memTotal)
	diskUsed := pct(s.diskUsed, s.diskUsed+s.diskFree)
	c := &spec.Component{
		ID: "host", Title: "Host", Group: "System", Description: "The box itself: load, memory, disk, uptime, failed units, OOM kills, pending reboot.",
		UpdatedAt: now, ExpectEvery: "5m",
	}
	g := func(key, label string, value float64, unit string, primary bool) {
		c.Gauges = append(c.Gauges, spec.Metric{Key: key, Label: label, Value: value, Unit: unit, Primary: primary})
	}
	g("load.1", "Load (1 min)", s.load[0], "", true)
	g("mem.used", "Memory used", memUsed, "percent", true)
	g("disk.used", "Disk used", diskUsed, "percent", true)
	g("uptime", "Uptime", s.uptime, "s", true)
	g("cpus", "CPUs", float64(runtime.NumCPU()), "count", false)
	g("load.5", "Load (5 min)", s.load[1], "", false)
	g("load.15", "Load (15 min)", s.load[2], "", false)
	g("mem.total", "Memory", s.memTotal, "bytes", false)
	g("mem.available", "Memory available", s.memAvail, "bytes", false)
	g("swap.total", "Swap", s.swapTotal, "bytes", false)
	g("swap.used", "Swap used", pct(s.swapUsed, s.swapTotal), "percent", false)
	g("disk.total", "Disk", s.diskTotal, "bytes", false)
	g("disk.free", "Disk free", s.diskFree, "bytes", false)
	c.Checks = []spec.Check{threshold("disk", diskUsed, 85, 95), threshold("memory", memUsed, 90, 98)}
	summary := fmt.Sprintf("disk %.0f%%, memory %.0f%%, load %.2f, up %s", diskUsed, memUsed, s.load[0], humanUptime(s.uptime))
	memo := in.Memo
	if s.unitsKnown {
		k, n := unitsCheck(s.failedUnits, in.Owned)
		g("units.failed", "Failed units", float64(len(s.failedUnits)), "count", false)
		c.Checks = append(c.Checks, k)
		if n > 0 {
			summary += fmt.Sprintf(", %d failed unit%s", n, plural(n))
		}
	}
	if s.oomKnown {
		var k spec.Check
		k, memo = oomCheck(now, s.oomKills, in.BootID, memo, victims)
		g("oom.kills", "OOM kills since boot", float64(s.oomKills), "count", false)
		c.Checks = append(c.Checks, k)
		if k.Status != spec.OK {
			summary += ", OOM kill in 24 h"
		}
	}
	if s.rebootKnown {
		var k spec.Check
		k, memo = rebootCheck(now, s, in.BootID, memo)
		c.Checks = append(c.Checks, k)
		if s.rebootPending {
			summary += ", reboot pending"
		}
	}
	memo.Boot = in.BootID
	c.Status = worst(c.Checks)
	c.Summary = summary
	return c, memo
}

func pluralES(n uint64) string {
	if n == 1 {
		return ""
	}
	return "es"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// unitsCheck warns on failed units no component watches (theirs fail their component);
// it returns how many it warns about.
func unitsCheck(failed []string, owned map[string]bool) (spec.Check, int) {
	var mine, theirs []string
	for _, u := range failed {
		if owned[u] {
			theirs = append(theirs, u)
		} else {
			mine = append(mine, u)
		}
	}
	k := spec.Check{Name: "units", Status: spec.OK, Detail: "no failed units"}
	if len(mine) > 0 {
		k.Status = spec.Warn
		k.Detail = fmt.Sprintf("%d failed: %s", len(mine), strings.Join(mine, ", "))
	} else if len(theirs) > 0 {
		k.Detail = "none outside the components"
	}
	if len(theirs) > 0 {
		k.Detail += fmt.Sprintf(" (their components show %s)", strings.Join(theirs, ", "))
	}
	k.Detail = truncate(k.Detail, 500)
	return k, len(mine)
}

// oomCheck compares the kernel's OOM kill counter with the last collect's and warns while
// a kill is younger than OOMWindow. The first reading is only a baseline (the counter
// covers the whole boot); after a reboot the counter starts again from zero.
func oomCheck(now time.Time, kills uint64, boot string, memo statefile.Host, victims func(since time.Time) []string) (spec.Check, statefile.Host) {
	sameBoot := memo.Boot != "" && memo.Boot == boot
	prev, baseline := memo.OOMKills, sameBoot
	if memo.Boot != "" && !sameBoot {
		prev, baseline = 0, true // a new boot: everything counted is new
	}
	if baseline && kills > prev {
		kill := statefile.OOMKill{At: now, Count: kills - prev}
		if victims != nil {
			kill.Victims = victims(now.Add(-2 * time.Minute)) // collect runs every minute
		}
		memo.OOM = append(memo.OOM, kill)
	}
	memo.OOMKills = kills
	memo.OOM = slices.DeleteFunc(memo.OOM, func(k statefile.OOMKill) bool { return now.Sub(k.At) >= OOMWindow || k.At.After(now) })
	k := spec.Check{Name: "oom", Status: spec.OK, Detail: "no OOM kills in the last 24 h"}
	if len(memo.OOM) > 0 {
		var n uint64
		var who []string
		for _, o := range memo.OOM {
			n += o.Count
			for _, v := range o.Victims {
				if !slices.Contains(who, v) {
					who = append(who, v)
				}
			}
		}
		last := memo.OOM[len(memo.OOM)-1].At
		k.Status = spec.Warn
		k.Detail = fmt.Sprintf("%d process%s killed for lack of memory in the last 24 h, the last seen %s", n, pluralES(n), last.Format(time.RFC3339))
		if len(who) > 0 {
			k.Detail += ": " + strings.Join(who, ", ")
		}
		k.Detail = truncate(k.Detail, 500)
	}
	return k, memo
}

// rebootCheck: a pending reboot is expected after updates and the box's own schedule
// reboots it (ok); one pending through RebootNights midnights (UTC) means those reboots
// aren't happening (warn).
func rebootCheck(now time.Time, s sample, boot string, memo statefile.Host) (spec.Check, statefile.Host) {
	if !s.rebootPending {
		memo.RebootSince = time.Time{}
		return spec.Check{Name: "reboot", Status: spec.OK, Detail: "none pending"}, memo
	}
	since := memo.RebootSince
	if memo.Boot != boot || since.After(now) {
		since = time.Time{} // the file is in /run: one from before this boot is gone
	}
	// The file's time is when the last update asked for a reboot: an earlier first one
	// may only be remembered (the file is rewritten on every such update).
	if at := s.rebootAt.UTC().Truncate(time.Second); !at.IsZero() && !at.After(now) && (since.IsZero() || at.Before(since)) {
		since = at
	}
	if since.IsZero() {
		since = now
	}
	memo.RebootSince = since
	nights := midnights(since, now)
	k := spec.Check{Name: "reboot", Status: spec.OK, Detail: "pending since " + since.Format(time.RFC3339)}
	if nights >= RebootNights {
		k.Status = spec.Warn
		k.Detail += fmt.Sprintf(", through %d nights: the box isn't rebooting", nights)
	}
	if len(s.rebootPkgs) > 0 {
		k.Detail += " (" + strings.Join(s.rebootPkgs, ", ") + ")"
	}
	k.Detail = truncate(k.Detail, 500)
	return k, memo
}

// midnights counts the 00:00 UTC boundaries in (since, now].
func midnights(since, now time.Time) int {
	day := func(t time.Time) int64 { return t.UTC().Unix() / 86400 }
	if !now.After(since) {
		return 0
	}
	return int(day(now) - day(since))
}

// "Out of memory: Killed process 1234 (python3) total-vm:…", also "Memory cgroup out of memory: …".
var killedRE = regexp.MustCompile(`Killed process (\d+) \(([^)]*)\)`)

// parseVictims names the processes the kernel log says it killed, oldest first, at most 5.
func parseVictims(log string) []string {
	var out []string
	for _, m := range killedRE.FindAllStringSubmatch(log, -1) {
		v := fmt.Sprintf("%s (pid %s)", m[2], m[1])
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	if len(out) > 5 {
		out = out[len(out)-5:]
	}
	return out
}

func truncate(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

func threshold(name string, value, warn, fail float64) spec.Check {
	c := spec.Check{Name: name, Status: spec.OK, Detail: fmt.Sprintf("%.0f%% used", value)}
	switch {
	case value >= fail:
		c.Status = spec.Fail
	case value >= warn:
		c.Status = spec.Warn
	}
	return c
}

func worst(checks []spec.Check) spec.Status {
	rank := map[spec.Status]int{spec.OK: 0, spec.Unknown: 1, spec.Warn: 2, spec.Fail: 3}
	status := spec.OK
	for _, c := range checks {
		if rank[c.Status] > rank[status] {
			status = c.Status
		}
	}
	return status
}

// humanUptime is "3d 4h", "4h 11m" or "7m".
func humanUptime(seconds float64) string {
	minutes := int(seconds) / 60
	days, hours, mins := minutes/1440, minutes/60%24, minutes%60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}

// parseUnits takes the unit names from `systemctl list-units --plain --no-legend`.
func parseUnits(out string) []string {
	var units []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "●"))
		if len(f) > 0 && strings.Contains(f[0], ".") {
			units = append(units, f[0])
		}
	}
	return units
}
