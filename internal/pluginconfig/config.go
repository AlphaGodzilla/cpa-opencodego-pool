// Package pluginconfig decodes this plugin's own section of config.yaml.
//
// The host hands the plugin only `plugins.configs.<pluginID>` as YAML bytes, with
// `enabled` and `priority` forced in. Everything else is plugin-owned.
package pluginconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults applied when a field is absent.
const (
	DefaultBaseURLPrefix   = "https://opencode.ai"
	DefaultPollInterval    = 10 * time.Minute
	DefaultSessionTTL      = 24 * time.Hour
	DefaultMaxSessions     = 10000
	DefaultRequestTimeout  = 15 * time.Second
	DefaultStaleAfter      = 30 * time.Minute
	DefaultConfigPollEvery = 30 * time.Second
	DefaultConcurrency     = 4
	DefaultSuspectThrottle = 60 * time.Second

	// DefaultAuthor and DefaultRepository fill the host-facing metadata fields that
	// validPlugin() requires to be non-empty. example.com is the IANA reserved
	// documentation domain, so the default reads as "not published" instead of
	// pointing at somebody else's repository.
	DefaultAuthor     = "local"
	DefaultRepository = "https://example.com/opencodego-pool"
)

// Duration accepts either a Go duration string ("600s", "24h") or a bare number
// of seconds, because YAML authors reach for both.
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		return nil
	}
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a scalar, got %s", node.Tag)
	}
	text := strings.TrimSpace(node.Value)
	if text == "" {
		return nil
	}
	if parsed, errParse := time.ParseDuration(text); errParse == nil {
		*d = Duration(parsed)
		return nil
	}
	var seconds float64
	if _, errScan := fmt.Sscanf(text, "%f", &seconds); errScan == nil {
		*d = Duration(time.Duration(seconds * float64(time.Second)))
		return nil
	}
	return fmt.Errorf("invalid duration %q: expected a Go duration (600s) or seconds", text)
}

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// ExpandUserPath expands a leading ~ the way the host expands plugins.dir.
//
// Go's os package does not expand ~ — that is a shell feature — so a config_path
// written as ~/.cli-proxy-api/config.yaml would be stat'ed literally and fail with
// ENOENT, leaving the plugin registered but refusing to route.
//
// A path that cannot be resolved is returned unchanged rather than turned into an
// error: failing to expand must not stop the plugin from registering, because a
// registered-but-degraded plugin reports the problem on its status page, while a
// plugin that refuses to register is silent.
func ExpandUserPath(path string) string {
	// Only the bare home forms are expanded. A path like ~someone/config.yaml is
	// left alone on purpose: rewriting it to <my home>/someone/config.yaml would
	// silently point at an unintended file, whereas leaving it alone fails with a
	// stat error the status page shows.
	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, "~\\") {
		return path
	}
	homeDir, errHomeDir := os.UserHomeDir()
	if errHomeDir != nil || strings.TrimSpace(homeDir) == "" {
		return path
	}
	remainder := strings.TrimLeft(strings.TrimPrefix(path, "~"), "/\\")
	if remainder == "" {
		return filepath.Clean(homeDir)
	}
	normalized := strings.ReplaceAll(remainder, "\\", "/")
	return filepath.Clean(filepath.Join(homeDir, filepath.FromSlash(normalized)))
}

// Config is the plugin's own configuration section.
type Config struct {
	Enabled  bool `yaml:"enabled"`
	Priority int  `yaml:"priority"`

	// UsageURL is the opencode usage endpoint. Required; it is never derived from
	// the provider base-url, because the base-url form is a deployment detail.
	UsageURL string `yaml:"usage_url"`
	// ConfigPath points at CLIProxyAPI's config.yaml. Required: it is the only
	// channel that yields plaintext api-keys.
	ConfigPath string `yaml:"config_path"`

	BaseURLPrefix  string   `yaml:"base_url_prefix"`
	PollInterval   Duration `yaml:"poll_interval"`
	SessionTTL     Duration `yaml:"session_ttl"`
	MaxSessions    int      `yaml:"max_sessions"`
	RequestTimeout Duration `yaml:"request_timeout"`
	StaleAfter     Duration `yaml:"stale_after"`
	LogLevel       string   `yaml:"log_level"`

	// Author and Repository are host-facing metadata, and they are not cosmetic:
	// the host's validPlugin() rejects a registration whose Name, Version, Author or
	// GitHubRepository is empty. That rejection is invisible from the plugin's side —
	// plugin.register still answers OK — so these must never be blank. Point
	// Repository at your own fork once you have one.
	Author     string `yaml:"author"`
	Repository string `yaml:"repository"`

	ExcludeKeys []string          `yaml:"exclude_keys"`
	PinSessions map[string]string `yaml:"pin_sessions"`
}

// Parse decodes the plugin config section and applies defaults.
//
// A zero-length payload is valid: it yields the defaults plus the validation
// errors that stop the plugin from routing.
func Parse(raw []byte) (Config, error) {
	var cfg Config
	if len(strings.TrimSpace(string(raw))) > 0 {
		if errUnmarshal := yaml.Unmarshal(raw, &cfg); errUnmarshal != nil {
			return Config{}, fmt.Errorf("parse plugin config: %w", errUnmarshal)
		}
	}
	cfg.applyDefaults()
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if strings.TrimSpace(c.BaseURLPrefix) == "" {
		c.BaseURLPrefix = DefaultBaseURLPrefix
	}
	if c.PollInterval <= 0 {
		c.PollInterval = Duration(DefaultPollInterval)
	}
	if c.SessionTTL <= 0 {
		c.SessionTTL = Duration(DefaultSessionTTL)
	}
	if c.MaxSessions <= 0 {
		c.MaxSessions = DefaultMaxSessions
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = Duration(DefaultRequestTimeout)
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = Duration(DefaultStaleAfter)
	}
	if strings.TrimSpace(c.LogLevel) == "" {
		c.LogLevel = "warn"
	}
	if strings.TrimSpace(c.Author) == "" {
		c.Author = DefaultAuthor
	}
	if strings.TrimSpace(c.Repository) == "" {
		c.Repository = DefaultRepository
	}
	c.Author = strings.TrimSpace(c.Author)
	c.Repository = strings.TrimSpace(c.Repository)
	c.UsageURL = strings.TrimSpace(c.UsageURL)
	// Expand ~ here so every downstream consumer, including the status page, sees a
	// path that os.Stat can actually resolve.
	c.ConfigPath = ExpandUserPath(strings.TrimSpace(c.ConfigPath))
	c.BaseURLPrefix = strings.TrimSpace(c.BaseURLPrefix)
}

// Problems lists conditions that make routing unsafe. While the list is
// non-empty the plugin declines every scheduler.pick, so the host keeps its
// built-in behaviour instead of acting on a credential table it cannot trust.
func (c Config) Problems() []string {
	var problems []string
	if c.UsageURL == "" {
		problems = append(problems, "usage_url is required")
	}
	if c.ConfigPath == "" {
		problems = append(problems,
			"config_path is required: it is the only channel that exposes the plaintext api-keys")
	}
	return problems
}

// Pinned returns the reference pinned to a session, if any.
func (c Config) Pinned(session string) (string, bool) {
	if len(c.PinSessions) == 0 {
		return "", false
	}
	ref, ok := c.PinSessions[strings.TrimSpace(session)]
	if !ok {
		return "", false
	}
	return strings.TrimSpace(ref), true
}

func referenceMatches(ref, token, authID, apiKey string) bool {
	normalized := strings.TrimSpace(ref)
	if normalized == "" {
		return false
	}
	return normalized == token || normalized == authID || normalized == apiKey
}

// ReferenceMatches reports whether a config reference (api-key, auth id, or
// token) identifies the given credential.
func ReferenceMatches(ref, token, authID, apiKey string) bool {
	return referenceMatches(ref, token, authID, apiKey)
}
