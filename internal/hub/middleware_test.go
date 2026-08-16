package hub

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kitsyai/hey/internal/svc"
)

func testConfig() *Config {
	return &Config{
		Port: DefaultPort,
		CORS: CORSConfig{AllowOrigins: []string{
			"https://heypkv.ai", "https://*.heypkv.ai", "http://localhost:3000",
		}},
	}
}

func testPacks() *svc.PackSet {
	return &svc.PackSet{Packs: map[string]svc.Pack{
		"postgres": {Pack: "postgres", Protocol: "postgres-wire", Bridge: &svc.Bridge{Path: "/svc/postgres", Kind: "http-query-api"}},
		"vinci":    {Pack: "vinci", Protocol: "openai-http", CORS: svc.CORSPolicy{AddOrigins: []string{"https://example.com"}}},
	}}
}

func passthrough() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func TestEnvelope_RejectsBadHost(t *testing.T) {
	h := Envelope(testConfig(), testPacks(), passthrough())
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Host = "evil.example.com:8525"
	req.Header.Set(RequiredClientHeader, "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a rebound Host header", rec.Code)
	}
}

func TestEnvelope_AllowsLoopbackHost(t *testing.T) {
	h := Envelope(testConfig(), testPacks(), passthrough())
	for _, host := range []string{"127.0.0.1:8525", "localhost:8525", "[::1]:8525"} {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Host = host
		req.Header.Set(RequiredClientHeader, "1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("host %q: status = %d, want 200", host, rec.Code)
		}
	}
}

func TestEnvelope_RequiresClientHeader(t *testing.T) {
	h := Envelope(testConfig(), testPacks(), passthrough())
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Host = "127.0.0.1:8525"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 without %s", rec.Code, RequiredClientHeader)
	}
}

func TestEnvelope_AllowsMatchingOrigin(t *testing.T) {
	h := Envelope(testConfig(), testPacks(), passthrough())
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Host = "127.0.0.1:8525"
	req.Header.Set("Origin", "https://heypkv.ai")
	req.Header.Set(RequiredClientHeader, "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://heypkv.ai" {
		t.Errorf("Access-Control-Allow-Origin = %q", got)
	}
}

func TestEnvelope_AllowsWildcardSubdomain(t *testing.T) {
	h := Envelope(testConfig(), testPacks(), passthrough())
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Host = "127.0.0.1:8525"
	req.Header.Set("Origin", "https://app.heypkv.ai")
	req.Header.Set(RequiredClientHeader, "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("wildcard subdomain should be allowed, got %d", rec.Code)
	}
}

func TestEnvelope_WildcardNeverMatchesApexAlone(t *testing.T) {
	// The apex itself (https://heypkv.ai) is allowed only because it's also
	// listed explicitly in testConfig; a bare wildcard config should NOT
	// match the apex with no subdomain label.
	cfg := &Config{Port: DefaultPort, CORS: CORSConfig{AllowOrigins: []string{"https://*.onlywildcard.example"}}}
	if originAllowed("https://onlywildcard.example", cfg.CORS.AllowOrigins) {
		t.Error("a *.domain wildcard must not match the bare apex")
	}
	if !originAllowed("https://app.onlywildcard.example", cfg.CORS.AllowOrigins) {
		t.Error("a *.domain wildcard should match a real subdomain")
	}
}

func TestEnvelope_RejectsDisallowedOrigin(t *testing.T) {
	h := Envelope(testConfig(), testPacks(), passthrough())
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Host = "127.0.0.1:8525"
	req.Header.Set("Origin", "https://evil.example.com")
	req.Header.Set(RequiredClientHeader, "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a disallowed origin", rec.Code)
	}
}

func TestEnvelope_NoOriginBypassesCORSButStillNeedsHeader(t *testing.T) {
	// The desktop app's native fetch sends no Origin at all — nothing to
	// check there, but the required-header gate still applies.
	h := Envelope(testConfig(), testPacks(), passthrough())
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Host = "127.0.0.1:8525"
	req.Header.Set(RequiredClientHeader, "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for a no-Origin request with the header", rec.Code)
	}
}

func TestEnvelope_PreflightAllowedOriginWithPNA(t *testing.T) {
	h := Envelope(testConfig(), testPacks(), passthrough())
	req := httptest.NewRequest(http.MethodOptions, "/svc/vinci/v1/chat/completions", nil)
	req.Host = "127.0.0.1:8525"
	req.Header.Set("Origin", "https://heypkv.ai")
	req.Header.Set("Access-Control-Request-Private-Network", "true")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Private-Network"); got != "true" {
		t.Errorf("Access-Control-Allow-Private-Network = %q, want true", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got == "" {
		t.Error("preflight response must advertise allowed headers")
	}
}

func TestEnvelope_PreflightDisallowedOriginNoPNAGrant(t *testing.T) {
	h := Envelope(testConfig(), testPacks(), passthrough())
	req := httptest.NewRequest(http.MethodOptions, "/svc/vinci/v1/chat/completions", nil)
	req.Host = "127.0.0.1:8525"
	req.Header.Set("Origin", "https://evil.example.com")
	req.Header.Set("Access-Control-Request-Private-Network", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("preflight status = %d, want 403 for a disallowed origin", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Private-Network"); got != "" {
		t.Error("PNA must never be granted to a disallowed origin")
	}
}

func TestEnvelope_PerServiceAdditiveCORS(t *testing.T) {
	// vinci's pack.CORS().AddOrigins includes https://example.com — allowed
	// under /svc/vinci/... but must NOT leak into postgres's or the hub's own
	// origin allow-list.
	h := Envelope(testConfig(), testPacks(), passthrough())

	req := httptest.NewRequest(http.MethodGet, "/svc/vinci/v1/models", nil)
	req.Host = "127.0.0.1:8525"
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set(RequiredClientHeader, "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("vinci's additive CORS origin should be allowed under /svc/vinci/..., got %d", rec.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/svc/postgres/query", nil)
	req2.Host = "127.0.0.1:8525"
	req2.Header.Set("Origin", "https://example.com")
	req2.Header.Set(RequiredClientHeader, "1")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Errorf("vinci's additive origin must not leak into postgres's allow-list, got %d", rec2.Code)
	}

	req3 := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req3.Host = "127.0.0.1:8525"
	req3.Header.Set("Origin", "https://example.com")
	req3.Header.Set(RequiredClientHeader, "1")
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusForbidden {
		t.Errorf("vinci's additive origin must not leak into the hub's own endpoints, got %d", rec3.Code)
	}
}

func TestEnvelope_HubJSONPerServiceCORSOverride(t *testing.T) {
	cfg := testConfig()
	cfg.Services = map[string]ServiceConfig{
		"postgres": {CORS: svc.CORSPolicy{AddOrigins: []string{"https://partner.example"}}},
	}
	h := Envelope(cfg, testPacks(), passthrough())
	req := httptest.NewRequest(http.MethodGet, "/svc/postgres/query", nil)
	req.Host = "127.0.0.1:8525"
	req.Header.Set("Origin", "https://partner.example")
	req.Header.Set(RequiredClientHeader, "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("hub.json services.postgres.cors.add_origins should be honored, got %d", rec.Code)
	}
}
