//go:build !darwin && !linux

package cprobe

// Unit and MinLeak are unset: there is nothing to measure.
const (
	Unit    = ""
	MinLeak = 0
)

// MallocInUse reports false: this platform has no heap probe.
func MallocInUse() (uint64, bool) {
	return 0, false
}
