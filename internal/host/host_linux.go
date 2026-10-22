//go:build linux

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
)

const Supported = true

func read() (sample, error) {
	var s sample
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return s, err
	}
	f := strings.Fields(string(data))
	if len(f) < 3 {
		return s, fmt.Errorf("/proc/loadavg: unexpected %q", data)
	}
	for i := range s.load {
		if s.load[i], err = strconv.ParseFloat(f[i], 64); err != nil {
			return s, fmt.Errorf("/proc/loadavg: %w", err)
		}
	}
	mem, err := meminfo()
	if err != nil {
		return s, err
	}
	s.memTotal, s.memAvail = mem["MemTotal"], mem["MemAvailable"]
	s.swapTotal, s.swapUsed = mem["SwapTotal"], mem["SwapTotal"]-mem["SwapFree"]
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil {
		return s, fmt.Errorf("statfs /: %w", err)
	}
	bs := float64(st.Bsize)
	s.diskTotal, s.diskUsed, s.diskFree = float64(st.Blocks)*bs, float64(st.Blocks-st.Bfree)*bs, float64(st.Bavail)*bs
	if s.uptime, err = uptime(); err != nil {
		return s, err
	}
	_, err = os.Stat("/var/run/reboot-required")
	s.rebootPending = err == nil
	return s, nil
}

func meminfo() (map[string]float64, error) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	values := map[string]float64{}
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
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
	return values, sc.Err()
}

func uptime() (float64, error) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(data))
	if len(f) == 0 {
		return 0, fmt.Errorf("/proc/uptime: empty")
	}
	return strconv.ParseFloat(f[0], 64)
}

// ReadFacts reads the box's facts.
func ReadFacts() Facts {
	f := Facts{Arch: runtime.GOARCH}
	if data, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
				f.OS = strings.Trim(v, `"`)
			}
		}
	}
	if data, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		f.Kernel = strings.TrimSpace(string(data))
	}
	if data, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		f.BootID = strings.TrimSpace(string(data))
	}
	if up, err := uptime(); err == nil {
		// Rounded to the minute so the boot time doesn't wobble between reads.
		f.BootAt = time.Now().Add(-time.Duration(up * float64(time.Second))).UTC().Round(time.Minute)
	}
	return f
}
