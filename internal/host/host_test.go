package host

import (
	"strings"
	"testing"
	"time"

	"github.com/eemax/beacon/internal/spec"
	"github.com/eemax/beacon/internal/statefile"
)

func TestHostComponentIsValid(t *testing.T) {
	if !Supported {
		t.Skip("no host component on this platform")
	}
	c, _, err := Component(time.Now(), Input{BootID: "b1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("host component invalid: %v", err)
	}
	values := map[string]float64{}
	for _, g := range c.Gauges {
		values[g.Key] = g.Value
	}
	if values["cpus"] < 1 || values["disk.used"] <= 0 || values["disk.used"] > 100 || values["mem.total"] <= 0 || values["uptime"] <= 0 {
		t.Errorf("implausible values: %v", values)
	}
	if values["mem.used"] <= 0 || values["mem.used"] >= 100 {
		t.Errorf("implausible memory use: %v", values["mem.used"])
	}
	f := ReadFacts()
	if f.OS == "" || f.BootID == "" || f.BootAt.IsZero() || f.Kernel == "" {
		t.Errorf("facts incomplete: %+v", f)
	}
}

func TestThresholdsAndUptime(t *testing.T) {
	for value, want := range map[float64]string{50: "ok", 85: "warn", 94.9: "warn", 95: "fail"} {
		if got := threshold("disk", value, 85, 95).Status; string(got) != want {
			t.Errorf("%v%%: %s, want %s", value, got, want)
		}
	}
	for seconds, want := range map[float64]string{420: "7m", 15060: "4h 11m", 273600: "3d 4h"} {
		if got := humanUptime(seconds); got != want {
			t.Errorf("%v: %q, want %q", seconds, got, want)
		}
	}
}

func TestUnitsCheck(t *testing.T) {
	k, n := unitsCheck(nil, nil)
	if k.Status != spec.OK || n != 0 || k.Detail != "no failed units" {
		t.Errorf("none: %+v", k)
	}
	owned := map[string]bool{"centric-fetch.service": true}
	k, n = unitsCheck([]string{"centric-fetch.service"}, owned)
	if k.Status != spec.OK || n != 0 || !strings.Contains(k.Detail, "centric-fetch.service") {
		t.Errorf("only a component's: %+v", k)
	}
	k, n = unitsCheck([]string{"centric-fetch.service", "apt-daily.service", "foo.timer"}, owned)
	if k.Status != spec.Warn || n != 2 || !strings.HasPrefix(k.Detail, "2 failed: apt-daily.service, foo.timer") {
		t.Errorf("others: %+v (%d)", k, n)
	}
	if got := parseUnits("apt-daily.service loaded failed failed Daily apt\n\nfoo.timer loaded failed failed Foo\n"); strings.Join(got, ",") != "apt-daily.service,foo.timer" {
		t.Errorf("parseUnits: %v", got)
	}
}

func TestOOMCheck(t *testing.T) {
	t0 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	calls := 0
	victims := func(time.Time) []string { calls++; return []string{"python3 (pid 42)"} }
	// The first reading is a baseline: kills from earlier in the boot don't warn.
	k, m := oomCheck(t0, 3, "b1", statefile.Host{}, victims)
	m.Boot = "b1"
	if k.Status != spec.OK || m.OOMKills != 3 || calls != 0 {
		t.Fatalf("baseline: %+v %+v", k, m)
	}
	k, m = oomCheck(t0.Add(time.Minute), 3, "b1", m, victims)
	if k.Status != spec.OK || calls != 0 {
		t.Fatalf("unchanged: %+v", k)
	}
	t1 := t0.Add(2 * time.Minute)
	k, m = oomCheck(t1, 5, "b1", m, victims)
	if k.Status != spec.Warn || calls != 1 || !strings.Contains(k.Detail, "2 processes") || !strings.Contains(k.Detail, "python3 (pid 42)") || !strings.Contains(k.Detail, t1.Format(time.RFC3339)) {
		t.Fatalf("kill: %+v", k)
	}
	k, m = oomCheck(t1.Add(23*time.Hour), 5, "b1", m, victims)
	if k.Status != spec.Warn {
		t.Fatalf("within 24 h: %+v", k)
	}
	k, m = oomCheck(t1.Add(24*time.Hour), 5, "b1", m, victims)
	if k.Status != spec.OK || len(m.OOM) != 0 {
		t.Fatalf("after 24 h: %+v %+v", k, m)
	}
	// After a reboot the counter starts from zero: any kill counted is new.
	k, m = oomCheck(t1.Add(25*time.Hour), 1, "b2", m, victims)
	if k.Status != spec.Warn || !strings.Contains(k.Detail, "1 process killed") {
		t.Fatalf("new boot: %+v", k)
	}
	if got := parseVictims("Out of memory: Killed process 1234 (node) total-vm:1kB\nMemory cgroup out of memory: Killed process 99 (python3) total-vm:2kB"); strings.Join(got, ",") != "node (pid 1234),python3 (pid 99)" {
		t.Errorf("parseVictims: %v", got)
	}
}

func TestRebootCheck(t *testing.T) {
	// Pending since 2026-11-01 23:30 UTC (the update came after the nightly window).
	since := time.Date(2026, 11, 1, 23, 30, 0, 0, time.UTC)
	s := sample{rebootKnown: true, rebootPending: true, rebootAt: since, rebootPkgs: []string{"linux-image-6.8.0-147-generic"}}
	m := statefile.Host{Boot: "b1"}
	for _, c := range []struct {
		now  time.Time
		want spec.Status
	}{
		{since.Add(10 * time.Minute), spec.OK},                    // midnight 1 ahead
		{time.Date(2026, 11, 3, 23, 59, 0, 0, time.UTC), spec.OK}, // 2 nights
		{time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC), spec.Warn}, // the 3rd
		{time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), spec.Warn},
	} {
		var k spec.Check
		k, m = rebootCheck(c.now, s, "b1", m)
		if k.Status != c.want || !strings.Contains(k.Detail, "pending since 2026-11-01T23:30:00Z") || !strings.Contains(k.Detail, "linux-image") {
			t.Errorf("%s: %+v", c.now, k)
		}
	}
	// A later update rewrites the file: the first time is remembered.
	s.rebootAt = time.Date(2026, 11, 5, 6, 0, 0, 0, time.UTC)
	if k, m2 := rebootCheck(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), s, "b1", m); k.Status != spec.Warn || !m2.RebootSince.Equal(since) {
		t.Errorf("rewritten file: %+v %v", k, m2.RebootSince)
	}
	// Rebooted: a new pending reboot starts over.
	if k, _ := rebootCheck(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), s, "b2", m); k.Status != spec.OK || !strings.Contains(k.Detail, "2026-11-05T06:00:00Z") {
		t.Errorf("new boot: %+v", k)
	}
	k, m := rebootCheck(time.Now(), sample{rebootKnown: true}, "b1", m)
	if k.Status != spec.OK || k.Detail != "none pending" || !m.RebootSince.IsZero() {
		t.Errorf("none: %+v %+v", k, m)
	}
}

func TestComponentChecks(t *testing.T) {
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	s := sample{memTotal: 4e9, memAvail: 2e9, diskTotal: 40e9, diskUsed: 10e9, diskFree: 30e9, uptime: 3600,
		rebootKnown: true, oomKnown: true, oomKills: 0, unitsKnown: true, failedUnits: []string{"x.service"}}
	c, memo := component(now, s, Input{BootID: "b1"}, nil)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	names := map[string]spec.Status{}
	for _, k := range c.Checks {
		names[k.Name] = k.Status
	}
	if names["units"] != spec.Warn || names["oom"] != spec.OK || names["reboot"] != spec.OK || c.Status != spec.Warn || memo.Boot != "b1" {
		t.Errorf("checks %v, status %s, memo %+v", names, c.Status, memo)
	}
	if !strings.Contains(c.Summary, "1 failed unit") {
		t.Errorf("summary: %q", c.Summary)
	}
	// macOS: none of the Linux checks.
	c, _ = component(now, sample{memTotal: 4e9, memAvail: 2e9, diskUsed: 1, diskFree: 1}, Input{}, nil)
	if len(c.Checks) != 2 {
		t.Errorf("checks without systemd: %+v", c.Checks)
	}
}
