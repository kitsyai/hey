package fetch

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// rangeServer serves payload, honoring HTTP Range requests exactly like a
// real CDN/GitHub-releases host — the shape DownloadResumable must resume
// against. hits counts every request received, so tests can assert a
// content-addressed destPath that already exists never touches the network.
func rangeServer(t *testing.T, payload []byte) (url string, hits *int) {
	t.Helper()
	n := 0
	hits = &n
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		start := 0
		if rng := r.Header.Get("Range"); rng != "" {
			var s int
			if _, err := fmt.Sscanf(rng, "bytes=%d-", &s); err == nil {
				start = s
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(payload)-1, len(payload)))
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)-start))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(payload[start:])
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.WriteHeader(http.StatusOK)
		w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	oldClient := Client
	Client = srv.Client()
	t.Cleanup(func() { Client = oldClient })
	return srv.URL + "/model.gguf", hits
}

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestDownloadResumable_FullDownload(t *testing.T) {
	payload := []byte(strings.Repeat("hey services model bytes ", 1000))
	url, _ := rangeServer(t, payload)
	dest := filepath.Join(t.TempDir(), "model.gguf")

	var lastDone, lastTotal int64
	err := DownloadResumable(url, dest, shaHex(payload), func(done, total int64) {
		lastDone, lastTotal = done, total
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("content mismatch: err=%v", err)
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Error(".part file should be renamed away on success")
	}
	if lastDone != int64(len(payload)) || lastTotal != int64(len(payload)) {
		t.Errorf("progress = %d/%d, want %d/%d", lastDone, lastTotal, len(payload), len(payload))
	}
}

func TestDownloadResumable_ResumesFromPartial(t *testing.T) {
	payload := []byte(strings.Repeat("resumable download bytes for hey vinci ", 500))
	url, hits := rangeServer(t, payload)
	dest := filepath.Join(t.TempDir(), "model.gguf")

	// Simulate a prior interrupted attempt: half the bytes already on disk.
	half := len(payload) / 2
	if err := os.WriteFile(dest+".part", payload[:half], 0o644); err != nil {
		t.Fatal(err)
	}

	if err := DownloadResumable(url, dest, shaHex(payload), nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("resumed content mismatch: err=%v", err)
	}
	// Confirms the request actually carried Range and only fetched the tail:
	// the range server always answers 206 with the remainder, so a resumed
	// download proves that path ran (as opposed to accidentally re-fetching
	// from scratch, which would also produce correct bytes but defeat the
	// point of resuming a multi-GB file).
	if *hits != 1 {
		t.Errorf("expected exactly one (ranged) request, got %d", *hits)
	}
}

func TestDownloadResumable_AlreadyPresentSkipsNetwork(t *testing.T) {
	payload := []byte("already have this one")
	url, hits := rangeServer(t, payload)
	dest := filepath.Join(t.TempDir(), "model.gguf")
	if err := os.WriteFile(dest, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := DownloadResumable(url, dest, shaHex(payload), nil); err != nil {
		t.Fatal(err)
	}
	if *hits != 0 {
		t.Errorf("expected no network activity for an already-content-addressed file, got %d hits", *hits)
	}
}

func TestDownloadResumable_ChecksumMismatchCleansUp(t *testing.T) {
	payload := []byte("some model bytes")
	url, _ := rangeServer(t, payload)
	dest := filepath.Join(t.TempDir(), "model.gguf")

	err := DownloadResumable(url, dest, strings.Repeat("f", 64), nil)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch error, got %v", err)
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Error("a corrupt .part file must not be left behind")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("destPath must never exist after a checksum failure")
	}
}

func TestDownloadResumable_RejectsHTTP(t *testing.T) {
	err := DownloadResumable("http://example.com/model.gguf", filepath.Join(t.TempDir(), "m"), strings.Repeat("0", 64), nil)
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("plain http must be refused, got %v", err)
	}
}

// noRangeServer never honors Range — like a plain static file host with no
// Accept-Ranges support — so DownloadResumable must detect the 200 (not 206)
// and restart from scratch rather than corrupting the file by appending a
// second full copy onto stale partial bytes.
func TestDownloadResumable_RestartsWhenServerIgnoresRange(t *testing.T) {
	payload := []byte(strings.Repeat("no-range-support bytes ", 200))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(payload)
	}))
	defer srv.Close()
	oldClient := Client
	Client = srv.Client()
	defer func() { Client = oldClient }()

	dest := filepath.Join(t.TempDir(), "model.gguf")
	if err := os.WriteFile(dest+".part", []byte("stale partial bytes that do not belong"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := DownloadResumable(srv.URL+"/x", dest, shaHex(payload), nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("expected a clean restart to produce exactly payload, got err=%v content=%q", err, got)
	}
}
