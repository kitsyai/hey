package pkgreg

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// writeStubTokenHelper builds a tiny real executable (not a shell/batch
// script routed through an interpreter) that prints token to stdout and
// appends to a counter file each time it runs, so the test can both verify
// the exact production code path (Core.BearerToken execs the helper path
// directly, no shell) and assert the ~5min cache avoids re-execing it. A
// compiled Go program is used instead of a .sh/.bat file so the exact same
// test works unmodified on every platform hey ships.
func writeStubTokenHelper(t *testing.T, token string) (path string, callCount func() int) {
	t.Helper()
	dir := t.TempDir()
	counter := filepath.Join(dir, "calls")
	src := filepath.Join(dir, "helper.go")

	program := fmt.Sprintf(`package main

import ("fmt";"os")

func main() {
	f, _ := os.OpenFile(%q, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	f.WriteString("x")
	f.Close()
	fmt.Println(%q)
}
`, counter, token)
	if err := os.WriteFile(src, []byte(program), 0o644); err != nil {
		t.Fatal(err)
	}

	exeName := "helper"
	if runtime.GOOS == "windows" {
		exeName = "helper.exe"
	}
	exePath := filepath.Join(dir, exeName)
	build := exec.Command("go", "build", "-o", exePath, src)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build stub token helper: %v\n%s", err, out)
	}

	callCount = func() int {
		b, err := os.ReadFile(counter)
		if err != nil {
			return 0
		}
		n := 0
		for _, c := range b {
			if c == 'x' {
				n++
			}
		}
		return n
	}
	return exePath, callCount
}

func TestBearerToken_ExecutesAndCaches(t *testing.T) {
	helper, calls := writeStubTokenHelper(t, "test-token-123")
	c := &Core{Client: http.DefaultClient}

	tok, err := c.BearerToken("example.com", helper)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "test-token-123" {
		t.Errorf("token = %q", tok)
	}
	if n := calls(); n != 1 {
		t.Errorf("helper called %d times, want 1", n)
	}

	// Second call within the TTL should hit the cache, not re-exec.
	tok2, err := c.BearerToken("example.com", helper)
	if err != nil {
		t.Fatal(err)
	}
	if tok2 != "test-token-123" {
		t.Errorf("cached token = %q", tok2)
	}
	if n := calls(); n != 1 {
		t.Errorf("helper called %d times after cached hit, want 1", n)
	}
}

func TestBearerToken_NoHelperNoAuth(t *testing.T) {
	c := &Core{Client: http.DefaultClient}
	tok, err := c.BearerToken("registry.npmjs.org", "")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "" {
		t.Errorf("token = %q, want empty (no auth needed)", tok)
	}
}

func TestProxyGet_StreamsAndNotFound(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte("hello tarball bytes"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	c := &Core{Client: upstream.Client()}

	rec := httptest.NewRecorder()
	if err := c.ProxyGet(rec, upstream.URL+"/ok", ""); err != nil {
		t.Fatal(err)
	}
	if rec.Body.String() != "hello tarball bytes" {
		t.Errorf("body = %q", rec.Body.String())
	}

	err := c.ProxyGet(httptest.NewRecorder(), upstream.URL+"/missing", "")
	if err == nil || !IsNotFound(err) {
		t.Errorf("expected IsNotFound error, got %v", err)
	}
}

func TestFetchBytes_NotFoundTolerant(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()
	c := &Core{Client: upstream.Client()}

	_, err := c.FetchBytes(upstream.URL+"/anything", "")
	if !IsNotFound(err) {
		t.Errorf("expected IsNotFound, got %v", err)
	}
}

func TestNewCore_CreatesStoreRoot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "store")
	core, err := NewCore(dir, 4874)
	if err != nil {
		t.Fatal(err)
	}
	if core.StoreRoot != dir {
		t.Errorf("StoreRoot = %q", core.StoreRoot)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Errorf("store root not created: %v", err)
	}
}
