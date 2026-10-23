package systemd

import "time"

type Unit struct {
	Name         string
	Type         string
	InvocationID string
	ActiveState  string // active, activating, deactivating, inactive, failed, …
	Result       string // success, exit-code, signal, timeout, …
	Start, Exit  time.Time
	ExitCode     int // ExecMainCode: 1 exited, 2 killed, 3 dumped
	ExitStatus   int // exit status, or the signal number when killed
	// Services.
	SubState    string
	ActiveSince time.Time // ActiveEnterTimestamp
	Restarts    int       // NRestarts: automatic restarts (Restart=) since it was last started
	Memory      int64     // MemoryCurrent in bytes, -1 when not known
}

// Job reports whether the unit runs to completion (a oneshot job) rather than staying up.
func (u Unit) Job() bool { return u.Type == "oneshot" }

// Running reports whether an invocation is in progress (a oneshot job is "activating").
func (u Unit) Running() bool {
	return u.ActiveState == "activating" || u.ActiveState == "active" || u.ActiveState == "deactivating" || u.ActiveState == "reloading"
}

type Timer struct {
	Unit      string
	Activates string
	Next      time.Time // zero when it won't elapse again
	Last      time.Time
}
