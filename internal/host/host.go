// Package host is the box itself: the built-in "host" component (load, memory, swap,
// disk, uptime, pending reboot) and the box's facts (OS, kernel, boot), on Linux and macOS.
package host

import (
	"fmt"
	"runtime"
	"time"

	"github.com/eemax/beacon/internal/spec"
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
	rebootPending                 bool
}

// Component reads the host now.
func Component(now time.Time) (*spec.Component, error) {
	s, err := read()
	if err != nil {
		return nil, err
	}
	pct := func(part, total float64) float64 {
		if total <= 0 {
			return 0
		}
		return float64(int(part/total*1000+0.5)) / 10
	}
	memUsed := pct(s.memTotal-s.memAvail, s.memTotal)
	diskUsed := pct(s.diskUsed, s.diskUsed+s.diskFree)
	c := &spec.Component{
		ID: "host", Title: "Host", Group: "System", Description: "The box itself: load, memory, disk, uptime.",
		UpdatedAt: now.UTC().Truncate(time.Second), ExpectEvery: "5m",
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
	if s.rebootPending {
		// Expected after updates; the box's own schedule handles it. Shown, not alarmed.
		c.Checks = append(c.Checks, spec.Check{Name: "reboot", Status: spec.OK, Detail: "pending (updates installed)"})
	}
	c.Status = worst(c.Checks)
	c.Summary = fmt.Sprintf("disk %.0f%%, memory %.0f%%, load %.2f, up %s", diskUsed, memUsed, s.load[0], humanUptime(s.uptime))
	return c, nil
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
