//go:build darwin

package host

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const Supported = true

func sysctlBytes(name string) ([]byte, error) {
	s, err := syscall.Sysctl(name)
	if err != nil {
		return nil, fmt.Errorf("sysctl %s: %w", name, err)
	}
	return []byte(s), nil
}

// syscall.Sysctl drops a trailing NUL byte; pad structs back to their size.
func pad(b []byte, n int) []byte {
	for len(b) < n {
		b = append(b, 0)
	}
	return b
}

func bootTime() (time.Time, error) {
	b, err := sysctlBytes("kern.boottime") // struct timeval { int64 sec; int32 usec }
	if err != nil {
		return time.Time{}, err
	}
	b = pad(b, 16)
	return time.Unix(int64(binary.LittleEndian.Uint64(b[0:8])), 0).UTC(), nil
}

func read() (sample, error) {
	var s sample
	b, err := sysctlBytes("vm.loadavg") // struct loadavg { uint32 ldavg[3]; long fscale }
	if err != nil {
		return s, err
	}
	b = pad(b, 24)
	scale := float64(binary.LittleEndian.Uint64(b[16:24]))
	for i := range s.load {
		if scale > 0 {
			s.load[i] = float64(binary.LittleEndian.Uint32(b[i*4:])) / scale
		}
	}
	if b, err := sysctlBytes("hw.memsize"); err == nil { // uint64
		s.memTotal = float64(binary.LittleEndian.Uint64(pad(b, 8)))
	}
	s.memAvail = vmAvailable(s.memTotal)
	if b, err := sysctlBytes("vm.swapusage"); err == nil { // struct xsw_usage { uint64 total, avail, used; … }
		b = pad(b, 24)
		s.swapTotal, s.swapUsed = float64(binary.LittleEndian.Uint64(b[0:8])), float64(binary.LittleEndian.Uint64(b[16:24]))
	}
	// The data volume is what fills up on APFS; "/" is the read-only system volume.
	root := "/System/Volumes/Data"
	if _, err := os.Stat(root); err != nil {
		root = "/"
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(root, &st); err != nil {
		return s, fmt.Errorf("statfs %s: %w", root, err)
	}
	bs := float64(st.Bsize)
	s.diskTotal, s.diskUsed, s.diskFree = float64(st.Blocks)*bs, float64(st.Blocks-st.Bfree)*bs, float64(st.Bavail)*bs
	if boot, err := bootTime(); err == nil {
		s.uptime = time.Since(boot).Seconds()
	}
	return s, nil
}

// vmAvailable: free + inactive + speculative + purgeable pages, from vm_stat.
func vmAvailable(total float64) float64 {
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return total
	}
	page := 4096.0
	var pages float64
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "page size of") {
			for _, f := range strings.Fields(line) {
				if n, err := strconv.Atoi(f); err == nil {
					page = float64(n)
				}
			}
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "Pages free", "Pages inactive", "Pages speculative", "Pages purgeable":
			n, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(v), "."), 64)
			pages += n
		}
	}
	return pages * page
}

// ReadFacts reads the box's facts.
func ReadFacts() Facts {
	f := Facts{Arch: runtime.GOARCH}
	name, _ := exec.Command("sw_vers", "-productName").Output()
	ver, _ := exec.Command("sw_vers", "-productVersion").Output()
	f.OS = strings.TrimSpace(strings.TrimSpace(string(name)) + " " + strings.TrimSpace(string(ver)))
	if t, err := syscall.Sysctl("kern.ostype"); err == nil {
		r, _ := syscall.Sysctl("kern.osrelease")
		f.Kernel = strings.TrimSpace(t + " " + r)
	}
	if id, err := syscall.Sysctl("kern.bootsessionuuid"); err == nil {
		f.BootID = id
	}
	if boot, err := bootTime(); err == nil {
		f.BootAt = boot
		if f.BootID == "" {
			f.BootID = strconv.FormatInt(boot.Unix(), 10)
		}
	}
	return f
}
