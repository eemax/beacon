//go:build !linux && !darwin

package host

import (
	"errors"
	"runtime"
)

const Supported = false

func read() (sample, error) {
	return sample{}, errors.New("the host component is only available on Linux and macOS")
}

func ReadFacts() Facts { return Facts{Arch: runtime.GOARCH} }
