package svc

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// State values for an instance.
const (
	StateRunning = "running"
	StateStopped = "stopped"
)

// Instance is the on-disk record of one provisioned service, stored as
// svc.json (mode 0600 — it holds generated credentials) in the instance dir.
type Instance struct {
	Name      string    `json:"name"`
	Pack      string    `json:"pack"`
	Version   string    `json:"version"`
	Driver    string    `json:"driver"`
	Platform  string    `json:"platform"`
	BinSubdir string    `json:"bin_subdir,omitempty"`
	Port      int       `json:"port"`
	User      string    `json:"user,omitempty"`
	Password  string    `json:"password,omitempty"`
	PID       int       `json:"pid,omitempty"`
	State     string    `json:"state"`
	Created   time.Time `json:"created"`
	Started   time.Time `json:"started,omitempty"`

	// ModelPath is the on-disk path of a model-server pack's ensured model
	// artifact (see EnsureModel), resolved into {model} for Start's command
	// template. Empty for every other driver.
	ModelPath string `json:"model_path,omitempty"`

	// ModelTier is the resolved Pack.Models key this instance is running
	// (empty for single-tier packs, or every other driver). Recorded so a
	// later `hey up vinci --model <different-tier>` against the same
	// instance name can be refused with a clear message instead of silently
	// ignoring the flag (see UpModel).
	ModelTier string `json:"model_tier,omitempty"`

	dir string // instance directory; not serialized
}

const svcFname = "svc.json"

// Dir returns the instance directory.
func (i *Instance) Dir() string { return i.dir }

// BinDir returns <instance>/bin — where the archive is extracted.
func (i *Instance) BinDir() string { return filepath.Join(i.dir, "bin") }

// ExeDir returns the directory that holds the executables ({bin}).
func (i *Instance) ExeDir() string { return filepath.Join(i.dir, "bin", i.BinSubdir) }

// DataDir returns <instance>/data — the durable data directory.
func (i *Instance) DataDir() string { return filepath.Join(i.dir, "data") }

// LogPath returns <instance>/logs/service.log.
func (i *Instance) LogPath() string { return filepath.Join(i.dir, "logs", "service.log") }

// vars returns the template variables for this instance (pwfile is passed in
// because it is transient and only exists during init).
func (i *Instance) vars(pwfile string) vars {
	return vars{
		bin: i.ExeDir(), data: i.DataDir(), port: i.Port,
		user: i.User, password: i.Password, pwfile: pwfile,
		model: i.ModelPath,
	}
}

// LoadInstance reads svc.json from dir.
func LoadInstance(dir string) (*Instance, error) {
	data, err := os.ReadFile(filepath.Join(dir, svcFname))
	if err != nil {
		return nil, err
	}
	var inst Instance
	if err := json.Unmarshal(data, &inst); err != nil {
		return nil, fmt.Errorf("parse %s: %w", filepath.Join(dir, svcFname), err)
	}
	inst.dir = dir
	return &inst, nil
}

// Save writes svc.json atomically with mode 0600 (it holds credentials).
func (i *Instance) Save() error {
	data, err := json.MarshalIndent(i, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(i.dir, svcFname)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	// os.WriteFile respects umask on create; force 0600 explicitly.
	_ = os.Chmod(tmp, 0o600)
	return os.Rename(tmp, path)
}

// randToken returns a random alphanumeric string of length n.
func randToken(n int) (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	out := make([]byte, n)
	for i := range out {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		out[i] = alphabet[idx.Int64()]
	}
	return string(out), nil
}

// genCredentials produces a fresh username and password. Credentials are never
// defaulted — a generated password is mandatory.
func genCredentials() (user, password string, err error) {
	suffix, err := randToken(6)
	if err != nil {
		return "", "", err
	}
	password, err = randToken(24)
	if err != nil {
		return "", "", err
	}
	return "hey_" + suffix, password, nil
}

// port range for services; 127.0.0.1 only. Chosen to sit above common defaults
// yet stay memorable; allocation records the exact port in svc.json.
const (
	portRangeLo = 5432
	portRangeHi = 5600
)

// allocatePort picks a free loopback TCP port from the service range, skipping
// ports already recorded by other instances. The chosen port is stable — it is
// persisted and reused across restarts.
func allocatePort(used map[int]bool) (int, error) {
	for p := portRangeLo; p <= portRangeHi; p++ {
		if used[p] {
			continue
		}
		if portFree(p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port in range %d-%d", portRangeLo, portRangeHi)
}

func portFree(p int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}
