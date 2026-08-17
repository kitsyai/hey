// Package sysmem reads available physical memory so callers (the model-server
// driver's pre-launch RAM check, internal/svc) can refuse to start a workload
// the machine cannot hold. Best-effort by design: on platforms where no
// dependency-free probe exists, Available returns ErrUnsupported and callers
// fail open — the check exists to stop a predictable out-of-memory crash, not
// to gate launches behind a measurement that might itself be wrong.
package sysmem

import "errors"

// ErrUnsupported is returned by Available on platforms without an
// implemented probe. Callers should treat it as "unknown" and fail open.
var ErrUnsupported = errors.New("sysmem: available-memory probe not supported on this platform")

// Available returns the bytes of physical memory currently available for new
// allocations without swapping (Windows: GlobalMemoryStatusEx ullAvailPhys;
// Linux: /proc/meminfo MemAvailable).
func Available() (uint64, error) { return available() }
