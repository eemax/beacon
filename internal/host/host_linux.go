//go:build linux

// Package host builds the "host" feed: load, memory, swap, disk and uptime of the box.
package host

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/eemax/beacon/internal/feed"
)

// Supported reports whether this platform has a host collector.
const Supported = true

// Feed reads the host's state now. Disk is the filesystem holding root.
func Feed(root string, now time.Time) (*feed.Feed, error) {
	f := &feed.Feed{
		Schema: feed.Schema, Feed: "host", Title: "Host", UpdatedAt: now.UTC(),
		ExpectEvery: "5m", Status: feed.OK,
	}
	add := func(key, label string, value float64, unit string) {
		f.Metrics = append(f.Metrics, feed.Metric{Key: key, Label: label, Value: value, Unit: unit})
	}

	load, err := loadavg()
	if err != nil {
		return nil, err
	}
	mem, err := meminfo()
	if err != nil {
		return nil, err
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(root, &st); err != nil {
		return nil, fmt.Errorf("statfs %s: %w", root, err)
	}
	uptime, err := uptimeSeconds()
	if err != nil {
		return nil, err
	}
	diskTotal := float64(st.Blocks) * float64(st.Bsize)
	diskFree := float64(st.Bavail) * float64(st.Bsize)
	// Like df: used / (used + available to users), so root's reserved blocks don't count.
	diskUsedBytes := float64(st.Blocks-st.Bfree) * float64(st.Bsize)
	diskUsed := percent(diskUsedBytes, diskUsedBytes+diskFree)
	memUsed := percent(mem["MemTotal"]-mem["MemAvailable"], mem["MemTotal"])
	swapUsed := percent(mem["SwapTotal"]-mem["SwapFree"], mem["SwapTotal"])
	_, rebootErr := os.Stat("/var/run/reboot-required")
	reboot := rebootErr == nil

	add("cpus", "CPUs", float64(runtime.NumCPU()), "count")
	add("load.1", "Load (1 min)", load[0], "")
	add("load.5", "Load (5 min)", load[1], "")
	add("load.15", "Load (15 min)", load[2], "")
	add("mem.total", "Memory", mem["MemTotal"], "bytes")
	add("mem.available", "Memory available", mem["MemAvailable"], "bytes")
	add("mem.used", "Memory used", memUsed, "percent")
	add("swap.total", "Swap", mem["SwapTotal"], "bytes")
	add("swap.used", "Swap used", swapUsed, "percent")
	add("disk.total", "Disk", diskTotal, "bytes")
	add("disk.free", "Disk free", diskFree, "bytes")
	add("disk.used", "Disk used", diskUsed, "percent")
	add("uptime", "Uptime", uptime, "s")
	add("reboot.required", "Reboot pending", boolValue(reboot), "")

	for i, m := range f.Metrics { // the headline numbers
		switch m.Key {
		case "disk.used", "mem.used", "load.1", "uptime":
			f.Metrics[i].Primary = true
		}
	}

	f.Checks = []feed.Check{
		threshold("disk", diskUsed, 85, 95),
		threshold("memory", memUsed, 90, 98),
	}
	if reboot {
		// Expected after updates; the box's own schedule handles it. Shown, not alarmed.
		f.Checks = append(f.Checks, feed.Check{Name: "reboot", Status: feed.OK, Detail: "pending (updates installed)"})
	}
	f.Status = worst(f.Checks)
	f.Summary = fmt.Sprintf("disk %.0f%%, memory %.0f%%, load %.2f, up %s",
		diskUsed, memUsed, load[0], humanUptime(uptime))
	return f, nil
}

func threshold(name string, value, warn, fail float64) feed.Check {
	c := feed.Check{Name: name, Status: feed.OK, Detail: fmt.Sprintf("%.0f%% used", value)}
	switch {
	case value >= fail:
		c.Status = feed.Fail
	case value >= warn:
		c.Status = feed.Warn
	}
	return c
}

func worst(checks []feed.Check) feed.Status {
	rank := map[feed.Status]int{feed.OK: 0, feed.Unknown: 1, feed.Warn: 2, feed.Fail: 3}
	status := feed.OK
	for _, c := range checks {
		if rank[c.Status] > rank[status] {
			status = c.Status
		}
	}
	return status
}

func loadavg() ([3]float64, error) {
	var load [3]float64
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return load, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return load, fmt.Errorf("/proc/loadavg: unexpected %q", data)
	}
	for i := range load {
		if load[i], err = strconv.ParseFloat(fields[i], 64); err != nil {
			return load, fmt.Errorf("/proc/loadavg: %w", err)
		}
	}
	return load, nil
}

// meminfo returns /proc/meminfo values in bytes.
func meminfo() (map[string]float64, error) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	values := map[string]float64{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, rest, ok := strings.Cut(scanner.Text(), ":")
		fields := strings.Fields(rest)
		if !ok || len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		if len(fields) > 1 && fields[1] == "kB" {
			n *= 1024
		}
		values[key] = n
	}
	for _, key := range []string{"MemTotal", "MemAvailable"} {
		if _, ok := values[key]; !ok {
			return nil, fmt.Errorf("/proc/meminfo: no %s", key)
		}
	}
	return values, scanner.Err()
}

func uptimeSeconds() (float64, error) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, fmt.Errorf("/proc/uptime: empty")
	}
	return strconv.ParseFloat(fields[0], 64)
}

// humanUptime is "3d 4h" or "4h 11m" or "7m".
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

func percent(part, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(int(part/total*1000+0.5)) / 10 // one decimal
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
