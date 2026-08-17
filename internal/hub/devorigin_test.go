package hub

import "testing"

// The embedded default hub.json must allow the localhost dev frontends,
// including the 4601/4602 dev ports that were previously rejected.
func TestDefaultConfig_AllowsLocalhostDevPorts(t *testing.T) {
	cfg, err := parseConfig(defaultConfigJSON, "embedded default")
	if err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{
		"http://localhost:4601",
		"http://localhost:4602",
		"http://localhost:3000",
		"http://localhost:5173",
	} {
		if !originAllowed(origin, cfg.CORS.AllowOrigins) {
			t.Errorf("default config rejects dev origin %s", origin)
		}
	}
	if originAllowed("http://localhost:9999", cfg.CORS.AllowOrigins) {
		t.Error("default config should not allow arbitrary localhost ports")
	}
}

func TestMatchOrigin_LoopbackPortWildcard(t *testing.T) {
	cases := []struct {
		origin, pattern string
		want            bool
	}{
		{"http://localhost:4601", "http://localhost:*", true},
		{"http://localhost:80", "http://localhost:*", true},
		{"http://127.0.0.1:4601", "http://127.0.0.1:*", true},
		{"http://127.0.0.1:4601", "http://localhost:*", false}, // host must match exactly
		{"http://localhost", "http://localhost:*", false},      // no port, no match
		{"http://localhost:", "http://localhost:*", false},
		{"http://localhost:4601x", "http://localhost:*", false},
		{"http://evil.example:8080", "http://evil.example:*", false}, // wildcard is loopback-only
		{"https://localhost:4601", "http://localhost:*", false},
	}
	for _, c := range cases {
		if got := matchOrigin(c.origin, c.pattern); got != c.want {
			t.Errorf("matchOrigin(%q, %q) = %v, want %v", c.origin, c.pattern, got, c.want)
		}
	}
}

func TestParseConfig_AppendsDevOriginsEnv(t *testing.T) {
	t.Setenv(DevOriginsEnv, "http://localhost:4603, http://127.0.0.1:4603 ,")
	cfg, err := parseConfig(defaultConfigJSON, "embedded default")
	if err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"http://localhost:4603", "http://127.0.0.1:4603"} {
		if !originAllowed(origin, cfg.CORS.AllowOrigins) {
			t.Errorf("HEY_HUB_DEV_ORIGINS origin %s not allowed", origin)
		}
	}
}
