package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/kitsyai/hey/internal/svc"
)

func TestNewHandler_Healthz(t *testing.T) {
	svcDir := t.TempDir()
	packs := &svc.PackSet{}
	h := NewHandler(svcDir, packs, testConfig(), DefaultPort, "0.1.0-dev")

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Host = "127.0.0.1:8525"
	req.Header.Set(RequiredClientHeader, "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestNewHandler_ServicesEmptyIsEmptyArrayNotNull(t *testing.T) {
	svcDir := t.TempDir()
	packs := &svc.PackSet{}
	h := NewHandler(svcDir, packs, testConfig(), DefaultPort, "0.1.0-dev")

	req := httptest.NewRequest(http.MethodGet, "/services", nil)
	req.Host = "127.0.0.1:8525"
	req.Header.Set(RequiredClientHeader, "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var cat Catalog
	if err := json.Unmarshal(rec.Body.Bytes(), &cat); err != nil {
		t.Fatal(err)
	}
	if cat.Services == nil {
		t.Error("Services must be [] not null when empty — a naive JS client would need a null check otherwise")
	}
	if !bytesContainArray(rec.Body.Bytes()) {
		t.Errorf(`raw body should contain "services":[] not "services":null, got %s`, rec.Body.String())
	}
}

func bytesContainArray(b []byte) bool {
	s := string(b)
	for i := 0; i+11 <= len(s); i++ {
		if s[i:i+11] == `"services":` {
			rest := s[i+11:]
			return len(rest) > 0 && rest[0] == '['
		}
	}
	return false
}

func TestNewHandler_ServicesListsProvisionedInstance(t *testing.T) {
	svcDir := t.TempDir()
	writeFakeInstance(t, svcDir, "postgres", "postgres", 5432, os.Getpid())
	packs := &svc.PackSet{Packs: map[string]svc.Pack{
		"postgres": {Pack: "postgres", Protocol: "postgres-wire", Bridge: &svc.Bridge{Path: "/svc/postgres", Kind: "http-query-api"}},
	}}
	h := NewHandler(svcDir, packs, testConfig(), DefaultPort, "0.1.0-dev")

	req := httptest.NewRequest(http.MethodGet, "/services", nil)
	req.Host = "127.0.0.1:8525"
	req.Header.Set(RequiredClientHeader, "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var cat Catalog
	if err := json.Unmarshal(rec.Body.Bytes(), &cat); err != nil {
		t.Fatal(err)
	}
	if len(cat.Services) != 1 || cat.Services[0].Name != "postgres" {
		t.Fatalf("Services = %+v", cat.Services)
	}
}

func TestNewHandler_ServiceOne_404ForUnknown(t *testing.T) {
	svcDir := t.TempDir()
	packs := &svc.PackSet{}
	h := NewHandler(svcDir, packs, testConfig(), DefaultPort, "0.1.0-dev")

	req := httptest.NewRequest(http.MethodGet, "/services/nope", nil)
	req.Host = "127.0.0.1:8525"
	req.Header.Set(RequiredClientHeader, "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestNewHandler_ServiceOne_Found(t *testing.T) {
	svcDir := t.TempDir()
	writeFakeInstance(t, svcDir, "vinci", "vinci", 51234, os.Getpid())
	packs := &svc.PackSet{Packs: map[string]svc.Pack{"vinci": {Pack: "vinci", Protocol: "openai-http"}}}
	h := NewHandler(svcDir, packs, testConfig(), DefaultPort, "0.1.0-dev")

	req := httptest.NewRequest(http.MethodGet, "/services/vinci", nil)
	req.Host = "127.0.0.1:8525"
	req.Header.Set(RequiredClientHeader, "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var view ServiceView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Name != "vinci" {
		t.Errorf("Name = %q", view.Name)
	}
}

func TestNewHandler_EnvelopeAppliedToEverything(t *testing.T) {
	// The envelope wraps the whole mux once (docs/design/services.md §5) — a
	// request with no required header must be rejected on /services and
	// /svc/... exactly as it is on /healthz, not just the hub's "own"
	// endpoints.
	svcDir := t.TempDir()
	packs := &svc.PackSet{Packs: map[string]svc.Pack{"vinci": {Pack: "vinci", Protocol: "openai-http"}}}
	h := NewHandler(svcDir, packs, testConfig(), DefaultPort, "0.1.0-dev")

	for _, path := range []string{"/services", "/services/vinci", "/svc/vinci/v1/models"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "127.0.0.1:8525"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s without %s: status = %d, want 400", path, RequiredClientHeader, rec.Code)
		}
	}
}
