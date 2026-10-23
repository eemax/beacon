//go:build linux

// Package systemd reads what systemd knows: for job units, each one's last invocation
// (InvocationID, start, exit, result, exit status) and the timers that start them; for
// services, whether they're up, since when, how often systemd restarted them and their
// memory; and the journal of one invocation.
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

var props = "Type,InvocationID,ActiveState,SubState,Result,ExecMainStartTimestamp,ExecMainExitTimestamp,ExecMainCode,ExecMainStatus,ActiveEnterTimestamp,NRestarts,MemoryCurrent,LoadState"

func Show(name string) (Unit, error) {
	out, err := exec.Command("systemctl", "show", name, "-p", props, "--timestamp=unix").Output()
	if err != nil {
		return Unit{}, fmt.Errorf("systemctl show %s: %w", name, err)
	}
	u := Unit{Name: name, Memory: -1}
	loaded := true
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
		case "SubState":
			u.SubState = v
		case "ActiveEnterTimestamp":
			u.ActiveSince = unix(v)
		case "NRestarts":
			u.Restarts, _ = strconv.Atoi(v)
		case "MemoryCurrent":
			// "[not set]", or 2^64-1 when accounting is off.
			if n, err := strconv.ParseUint(v, 10, 64); err == nil && n < 1<<62 {
				u.Memory = int64(n)
			}
		case "LoadState":
			loaded = v == "loaded"
		}
	}
	if !loaded {
		return u, fmt.Errorf("%s: no such unit", name)
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
