package pkgreg

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// PidInfo is the on-disk record of a background `hey registry start`,
// mirroring crow's crow.pid pattern (see apps/api/cmd/crow/main.go) so the
// ecosystem's background-service ergonomics stay consistent: one small JSON
// file, a TCP liveness probe, no OS-level service manager.
type PidInfo struct {
	PID   int    `json:"pid"`
	Port  int    `json:"port"`
	Store string `json:"store"`
}

// PidPath returns the pidfile path for a store root: <store>/registry.pid.
// It lives at the store root, one level above the per-format subdirectories
// (<store>/npm/...), since the running server is shared across formats.
func PidPath(storeRoot string) string {
	return filepath.Join(storeRoot, "registry.pid")
}

// WritePidFile records the running server's pid/port/store.
func WritePidFile(storeRoot string, info PidInfo) error {
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return os.WriteFile(PidPath(storeRoot), b, 0o644)
}

// ReadPidFile reads back a previously written pidfile, if any.
func ReadPidFile(storeRoot string) (PidInfo, bool) {
	b, err := os.ReadFile(PidPath(storeRoot))
	if err != nil {
		return PidInfo{}, false
	}
	var info PidInfo
	if json.Unmarshal(b, &info) != nil || info.PID == 0 {
		return PidInfo{}, false
	}
	return info, true
}

// RemovePidFile deletes the pidfile (best effort).
func RemovePidFile(storeRoot string) { _ = os.Remove(PidPath(storeRoot)) }

// ServerUp reports whether something is listening on 127.0.0.1:port.
func ServerUp(port int) bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
