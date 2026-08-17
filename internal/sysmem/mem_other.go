//go:build !windows && !linux

package sysmem

// available is unimplemented here (darwin needs host_statistics64 via cgo or
// a mach syscall shim — not worth a dependency for a best-effort check);
// callers fail open on ErrUnsupported.
func available() (uint64, error) { return 0, ErrUnsupported }
