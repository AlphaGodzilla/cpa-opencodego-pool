package pluginconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseAppliesDefaults(t *testing.T) {
	cfg, errParse := Parse([]byte("enabled: true\npriority: 100\n"))
	if errParse != nil {
		t.Fatalf("Parse: %v", errParse)
	}
	if !cfg.Enabled || cfg.Priority != 100 {
		t.Errorf("host-owned fields not read: %+v", cfg)
	}
	if cfg.BaseURLPrefix != DefaultBaseURLPrefix {
		t.Errorf("BaseURLPrefix = %q", cfg.BaseURLPrefix)
	}
	if cfg.PollInterval.Std() != DefaultPollInterval {
		t.Errorf("PollInterval = %v", cfg.PollInterval.Std())
	}
	if cfg.SessionTTL.Std() != DefaultSessionTTL {
		t.Errorf("SessionTTL = %v", cfg.SessionTTL.Std())
	}
	if cfg.MaxSessions != DefaultMaxSessions {
		t.Errorf("MaxSessions = %d", cfg.MaxSessions)
	}
	if cfg.RequestTimeout.Std() != DefaultRequestTimeout {
		t.Errorf("RequestTimeout = %v", cfg.RequestTimeout.Std())
	}
	if cfg.StaleAfter.Std() != DefaultStaleAfter {
		t.Errorf("StaleAfter = %v", cfg.StaleAfter.Std())
	}
	if cfg.LogLevel != "warn" {
		t.Errorf("LogLevel = %q", cfg.LogLevel)
	}
}

func TestParseAcceptsBothDurationForms(t *testing.T) {
	cfg, errParse := Parse([]byte(
		"poll_interval: 600s\nsession_ttl: 24h\nrequest_timeout: 90\nstale_after: \"45m\"\n"))
	if errParse != nil {
		t.Fatalf("Parse: %v", errParse)
	}
	if cfg.PollInterval.Std() != 10*time.Minute {
		t.Errorf("PollInterval = %v", cfg.PollInterval.Std())
	}
	if cfg.SessionTTL.Std() != 24*time.Hour {
		t.Errorf("SessionTTL = %v", cfg.SessionTTL.Std())
	}
	if cfg.RequestTimeout.Std() != 90*time.Second {
		t.Errorf("a bare number must mean seconds, got %v", cfg.RequestTimeout.Std())
	}
	if cfg.StaleAfter.Std() != 45*time.Minute {
		t.Errorf("StaleAfter = %v", cfg.StaleAfter.Std())
	}
}

func TestParseRejectsAMalformedDuration(t *testing.T) {
	if _, errParse := Parse([]byte("poll_interval: soon\n")); errParse == nil {
		t.Fatal("expected an error")
	}
}

func TestParseOfAnEmptySectionIsValid(t *testing.T) {
	cfg, errParse := Parse(nil)
	if errParse != nil {
		t.Fatalf("Parse: %v", errParse)
	}
	if cfg.UsageURL != "" || cfg.ConfigPath != "" {
		t.Errorf("unexpected values: %+v", cfg)
	}
}

func TestProblemsNameTheMissingFields(t *testing.T) {
	cfg, _ := Parse(nil)
	problems := cfg.Problems()
	if len(problems) != 2 {
		t.Fatalf("problems = %v, want two entries", problems)
	}

	complete, _ := Parse([]byte("usage_url: https://opencode.ai/zen/go/v1/usage\nconfig_path: /etc/cpa/config.yaml\n"))
	if remaining := complete.Problems(); len(remaining) != 0 {
		t.Fatalf("problems = %v, want none", remaining)
	}
}

func TestPinnedTrimsTheSessionKey(t *testing.T) {
	cfg, errParse := Parse([]byte("pin_sessions:\n  \"sess-1\": \"tok-a\"\n"))
	if errParse != nil {
		t.Fatalf("Parse: %v", errParse)
	}
	ref, ok := cfg.Pinned("  sess-1  ")
	if !ok || ref != "tok-a" {
		t.Fatalf("Pinned = %q/%v", ref, ok)
	}
	if _, ok := cfg.Pinned("sess-2"); ok {
		t.Error("an unknown session must not resolve")
	}
	if _, ok := (Config{}).Pinned("sess-1"); ok {
		t.Error("an empty pin table must not resolve")
	}
}

func TestReferenceMatches(t *testing.T) {
	if !ReferenceMatches("tok", "tok", "auth", "sk") {
		t.Error("token spelling should match")
	}
	if !ReferenceMatches("auth", "tok", "auth", "sk") {
		t.Error("auth id spelling should match")
	}
	if !ReferenceMatches("sk", "tok", "auth", "sk") {
		t.Error("api-key spelling should match")
	}
	if ReferenceMatches("nope", "tok", "auth", "sk") {
		t.Error("an unrelated reference must not match")
	}
	if ReferenceMatches("   ", "tok", "auth", "sk") {
		t.Error("a blank reference must not match")
	}
}

// TestParseExpandsATildeInConfigPath pins that a config_path written the way an
// operator naturally writes it actually resolves. Go's os package does not expand
// ~, so without this the plugin registers, then refuses to route with
// "config_path has not been read successfully" on its status page.
func TestParseExpandsATildeInConfigPath(t *testing.T) {
	homeDir, errHomeDir := os.UserHomeDir()
	if errHomeDir != nil || strings.TrimSpace(homeDir) == "" {
		t.Skip("no home directory available")
	}

	cases := map[string]string{
		"~/cpa/config.yaml":     filepath.Join(homeDir, "cpa/config.yaml"),
		"~/cpa/nested/x.yaml":   filepath.Join(homeDir, "cpa/nested/x.yaml"),
		"~":                     filepath.Clean(homeDir),
		"~/":                    filepath.Clean(homeDir),
		"/absolute/config.yaml": "/absolute/config.yaml",
		"relative/config.yaml":  "relative/config.yaml",
		// Another user's home must not be silently rewritten into ours.
		"~someone/config.yaml": "~someone/config.yaml",
	}
	for input, want := range cases {
		cfg, errParse := Parse([]byte("config_path: \"" + input + "\"\n"))
		if errParse != nil {
			t.Fatalf("Parse(%q): %v", input, errParse)
		}
		if cfg.ConfigPath != want {
			t.Errorf("config_path %q expanded to %q, want %q", input, cfg.ConfigPath, want)
		}
	}
}

func TestExpandUserPathWithoutAHomeDirectory(t *testing.T) {
	// A path that needs no expansion is returned verbatim.
	if got := ExpandUserPath("/etc/cpa/config.yaml"); got != "/etc/cpa/config.yaml" {
		t.Errorf("got %q", got)
	}
	if got := ExpandUserPath(""); got != "" {
		t.Errorf("got %q", got)
	}
}
