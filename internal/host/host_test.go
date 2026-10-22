package host

import (
	"testing"
	"time"
)

func TestHostComponentIsValid(t *testing.T) {
	if !Supported {
		t.Skip("no host component on this platform")
	}
	c, err := Component(time.Now())
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
