// Package statefile is beacon's own small state on the box (JSON, under a file lock): the
// last boot it saw, which systemd invocations it already logged, and runs in progress
// started by `beacon exec`.
package statefile

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type Marker struct {
	Component string    `json:"component"`
	StartedAt time.Time `json:"started_at"`
	Trigger   string    `json:"trigger,omitempty"`
	PID       int       `json:"pid"`
}

type State struct {
	BootID  string            `json:"boot_id,omitempty"`
	Kernel  string            `json:"kernel,omitempty"`
	Seen    map[string]string `json:"seen,omitempty"`    // systemd unit → last invocation logged
	Running map[string]Marker `json:"running,omitempty"` // run id → a `beacon exec` in progress
	// When each component's last run started (interval schedules without timers).
	LastStart map[string]time.Time `json:"last_start,omitempty"`
	Defined   []string             `json:"defined,omitempty"`  // definitions seen last collect
	Implicit  []string             `json:"implicit,omitempty"` // exec-only components written last collect
	PrunedAt  time.Time            `json:"pruned_at,omitempty"`
	// The last run adapter failure per component, until its adapter works again (shown as a
	// warning on the component: the run was logged without its stats).
	RunAdapter map[string]string `json:"run_adapter,omitempty"`
}

func read(path string) (State, error) {
	var s State
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &s); err != nil {
			return State{}, nil // corrupt: start over rather than stop working
		}
	}
	return s, nil
}

// Read returns the state without locking (a consistent snapshot: writes are atomic).
func Read(path string) (State, error) { return read(path) }

// Update applies fn to the state under an exclusive lock and saves it atomically.
func Update(path string, fn func(*State) error) error {
	lf, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	s, err := read(path)
	if err != nil {
		return err
	}
	if s.Seen == nil {
		s.Seen = map[string]string{}
	}
	if s.Running == nil {
		s.Running = map[string]Marker{}
	}
	if err := fn(&s); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state.*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
