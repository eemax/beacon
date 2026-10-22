//go:build linux

// Package systemd reads what systemd knows about job units: each unit's last invocation
// (InvocationID, start, exit, result, exit status), the timers that start them, and the
// journal of one invocation.
package systemd

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const Supported = true

type Unit struct {
	Name         string
	Type         string
	InvocationID string
	ActiveState  string // active, activating, deactivating, inactive, failed, …
	Result       string // success, exit-code, signal, timeout, …
	Start, Exit  time.Time
	ExitCode     int // ExecMainCode: 1 exited, 2 killed, 3 dumped
	ExitStatus   int // exit status, or the signal number when killed
}

// Running reports whether an invocation is in progress (a oneshot job is "activating").
func (u Unit) Running() bool {
	return u.ActiveState == "activating" || u.ActiveState == "active" || u.ActiveState == "deactivating" || u.ActiveState == "reloading"
}

var props = "Type,InvocationID,ActiveState,Result,ExecMainStartTimestamp,ExecMainExitTimestamp,ExecMainCode,ExecMainStatus"

func Show(name string) (Unit, error) {
	out, err := exec.Command("systemctl", "show", name, "-p", props, "--timestamp=unix").Output()
	if err != nil {
		return Unit{}, fmt.Errorf("systemctl show %s: %w", name, err)
	}
	u := Unit{Name: name}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "Type":
			u.Type = v
		case "InvocationID":
			u.InvocationID = v
		case "ActiveState":
			u.ActiveState = v
		case "Result":
			u.Result = v
		case "ExecMainStartTimestamp":
			u.Start = unix(v)
		case "ExecMainExitTimestamp":
			u.Exit = unix(v)
		case "ExecMainCode":
			u.ExitCode, _ = strconv.Atoi(v)
		case "ExecMainStatus":
			u.ExitStatus, _ = strconv.Atoi(v)
		}
	}
	return u, nil
}

// "@1792630804" → time; "" or "0" → zero.
func unix(v string) time.Time {
	n, err := strconv.ParseInt(strings.TrimPrefix(v, "@"), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}

type Timer struct {
	Unit      string
	Activates string
	Next      time.Time // zero when it won't elapse again
	Last      time.Time
}

func Timers() ([]Timer, error) {
	out, err := exec.Command("systemctl", "list-timers", "--all", "--output=json").Output()
	if err != nil {
		return nil, fmt.Errorf("systemctl list-timers: %w", err)
	}
	var raw []struct {
		Next      int64  `json:"next"`
		Last      int64  `json:"last"`
		Unit      string `json:"unit"`
		Activates string `json:"activates"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("systemctl list-timers: %w", err)
	}
	us := func(n int64) time.Time {
		if n <= 0 {
			return time.Time{}
		}
		return time.UnixMicro(n).UTC()
	}
	timers := make([]Timer, len(raw))
	for i, t := range raw {
		timers[i] = Timer{Unit: t.Unit, Activates: t.Activates, Next: us(t.Next), Last: us(t.Last)}
	}
	return timers, nil
}

// Journal returns the last n lines one invocation wrote.
func Journal(invocation string, n int) string {
	out, _ := exec.Command("journalctl", "_SYSTEMD_INVOCATION_ID="+invocation, "-n", strconv.Itoa(n), "-o", "cat", "--no-pager").Output()
	return strings.TrimSpace(string(out))
}
