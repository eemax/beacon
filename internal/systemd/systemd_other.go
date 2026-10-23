//go:build !linux

package systemd

import "errors"

const Supported = false

var errUnsupported = errors.New("systemd is only available on Linux (use beacon exec)")

func Show(string) (Unit, error)  { return Unit{}, errUnsupported }
func Timers() ([]Timer, error)   { return nil, errUnsupported }
func Journal(string, int) string { return "" }
