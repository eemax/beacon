//go:build !linux

package host

import (
	"errors"
	"time"

	"github.com/eemax/beacon/internal/feed"
)

// Supported reports whether this platform has a host collector.
const Supported = false

// Feed is only implemented on Linux.
func Feed(root string, now time.Time) (*feed.Feed, error) {
	return nil, errors.New("the host collector is only available on Linux")
}
