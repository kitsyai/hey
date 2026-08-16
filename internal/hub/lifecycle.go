package hub

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// PidInfo is the on-disk record of a background `hey hub start`, the exact
// pattern internal/pkgreg/lifecycle.go already ships for `hey registry` —
// see that file's doc comment: "mirror[s] crow's ergonomics" — the hub is a
// third instance of the same pidfile + TCP-liveness pattern (docs/design/
// services.md §4 "Registration mechanism").
type PidInfo struct {
	PID  int `json:"pid"`
	Port int `json:"port"`
}

// PidPath returns the hub's pidfile path: <hubDir>/hub.pid.
func PidPath(hubDir string) string { return filepath.Join(hubDir, "hub.pid") }

// WritePidFile records the running hub's pid/port.
func WritePidFile(hubDir string, info PidInfo) error {
	if err := os.MkdirAll(hubDir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return os.WriteFile(PidPath(hubDir), b, 0o644)
}

// ReadPidFile reads back a previously written pidfile, if any.
func ReadPidFile(hubDir string) (PidInfo, bool) {
	b, err := os.ReadFile(PidPath(hubDir))
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
func RemovePidFile(hubDir string) { _ = os.Remove(PidPath(hubDir)) }

// ServerUp reports whether something is listening on 127.0.0.1:port.
func ServerUp(port int) bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
