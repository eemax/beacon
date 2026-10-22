//go:build linux

package host

import (
	"testing"
	"time"
)

func TestHostFeedIsValid(t *testing.T) {
	f, err := Feed("/", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("host feed invalid: %v", err)
	}
	keys := map[string]float64{}
	for _, m := range f.Metrics {
		keys[m.Key] = m.Value
	}
	for _, key := range []string{"cpus", "load.1", "mem.used", "disk.used", "uptime"} {
		if _, ok := keys[key]; !ok {
			t.Errorf("missing metric %s", key)
		}
	}
	if keys["cpus"] < 1 || keys["disk.used"] <= 0 || keys["disk.used"] > 100 {
		t.Errorf("implausible values: %v", keys)
	}
}

func TestThresholds(t *testing.T) {
	cases := map[float64]string{50: "ok", 85: "warn", 94.9: "warn", 95: "fail"}
	for value, want := range cases {
		if got := threshold("disk", value, 85, 95).Status; string(got) != want {
			t.Errorf("%v%%: %s, want %s", value, got, want)
		}
	}
}

func TestHumanUptime(t *testing.T) {
	for seconds, want := range map[float64]string{420: "7m", 15060: "4h 11m", 273600: "3d 4h"} {
		if got := humanUptime(seconds); got != want {
			t.Errorf("%v: %q, want %q", seconds, got, want)
		}
	}
}
