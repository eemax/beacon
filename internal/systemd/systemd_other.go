//go:build !linux

package systemd

import (
	"errors"
	"time"
)

const Supported = false

type Unit struct {
	Name, Type, InvocationID, ActiveState, Result string
	Start, Exit                                   time.Time
	ExitCode, ExitStatus                          int
}

func (u Unit) Running() bool { return false }

type Timer struct {
	Unit, Activates string
	Next, Last      time.Time
}

var errUnsupported = errors.New("systemd is only available on Linux (use beacon exec)")

func Show(string) (Unit, error)  { return Unit{}, errUnsupported }
func Timers() ([]Timer, error)   { return nil, errUnsupported }
func Journal(string, int) string { return "" }
