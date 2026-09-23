package keysource

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleConfig = `
other: value
openai-compatibility:
  - name: opencodego
    base-url: https://opencode.ai/zen/go/v1
    api-key-entries:
      - api-key: sk-test-1
      - api-key: sk-test-2
        proxy-url: http://127.0.0.1:7890
      - api-key: ""
      - api-key: sk-test-1
  - name: plain
    base-url: https://api.example.com/v1
    api-key-entries:
      - api-key: sk-other
  - name: disabled-pool
    base-url: https://opencode.ai/zen/go/v2
    disabled: true
    api-key-entries:
      - api-key: sk-disabled
  - name: opencodego
    base-url: https://opencode.ai/zen/go/v1
    api-key-entries:
      - api-key: sk-test-3
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, []byte(content), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}
	return path
}

func TestLoadKeysFiltersAndResolves(t *testing.T) {
	path := writeConfig(t, sampleConfig)

	keys, errLoad := LoadKeys(path, "https://opencode.ai")
	if errLoad != nil {
		t.Fatalf("LoadKeys: %v", errLoad)
	}

	tokens := make([]string, 0, len(keys))
	for _, key := range keys {
		tokens = append(tokens, key.Token)
	}
	// sk-test-2 is configured with a proxy-url, which participates in the hash and
	// therefore yields a different credential from the bare sk-test-2 it resembles.
	want := []string{"c7b6e31b521d", "d2f411fd87e9", "c7b6e31b521d-1", "b897025fa5d8"}
	if strings.Join(tokens, ",") != strings.Join(want, ",") {
		t.Fatalf("tokens = %v, want %v", tokens, want)
	}

	// The proxy variant is a distinct credential and keeps its proxy.
	if keys[1].ProxyURL != "http://127.0.0.1:7890" {
		t.Errorf("proxy-url lost: %+v", keys[1])
	}
	if keys[0].ProxyURL != "" {
		t.Errorf("unexpected proxy-url: %+v", keys[0])
	}

	for _, key := range keys {
		if key.Provider != "openai-compatible-opencodego" {
			t.Errorf("provider = %q, want openai-compatible-opencodego", key.Provider)
		}
		if key.BaseURL != "https://opencode.ai/zen/go/v1" {
			t.Errorf("base url = %q", key.BaseURL)
		}
		if key.AuthID != "openai-compatibility:opencodego:"+key.Token {
			t.Errorf("auth id = %q for token %q", key.AuthID, key.Token)
		}
	}
}

func TestLoadKeysSkipsDisabledAndForeignProviders(t *testing.T) {
	path := writeConfig(t, sampleConfig)

	keys, errLoad := LoadKeys(path, "https://opencode.ai")
	if errLoad != nil {
		t.Fatalf("LoadKeys: %v", errLoad)
	}
	for _, key := range keys {
		if key.APIKey == "sk-disabled" {
			t.Errorf("a disabled provider must not contribute credentials")
		}
		if key.APIKey == "sk-other" {
			t.Errorf("a provider outside the prefix must not contribute credentials")
		}
	}
}

func TestLoadKeysWidensWithPrefix(t *testing.T) {
	path := writeConfig(t, sampleConfig)

	keys, errLoad := LoadKeys(path, "https://")
	if errLoad != nil {
		t.Fatalf("LoadKeys: %v", errLoad)
	}
	// opencodego (4 entries, 1 empty key dropped, 1 duplicate kept) + disabled-pool
	// is skipped by `disabled` + plain contributes sk-other.
	// With this prefix all three enabled providers match: opencodego contributes 3
	// keys (one empty api-key dropped, one duplicate kept), plain contributes 1,
	// the second opencodego block contributes 1, and disabled-pool is skipped.
	if len(keys) != 5 {
		got := make([]string, 0, len(keys))
		for _, key := range keys {
			got = append(got, key.APIKey)
		}
		t.Fatalf("got %d keys (%v), want 5", len(keys), got)
	}
}

func TestLoadKeysReportsReadErrors(t *testing.T) {
	if _, errLoad := LoadKeys(filepath.Join(t.TempDir(), "missing.yaml"), "https://opencode.ai"); errLoad == nil {
		t.Fatal("expected an error for a missing file")
	}
	path := writeConfig(t, "openai-compatibility: [this is not a list}\n")
	if _, errLoad := LoadKeys(path, "https://opencode.ai"); errLoad == nil {
		t.Fatal("expected an error for malformed yaml")
	}
}

func TestLoadKeysWithoutAPrefixMatchesEverything(t *testing.T) {
	path := writeConfig(t, sampleConfig)
	keys, errLoad := LoadKeys(path, "")
	if errLoad != nil {
		t.Fatalf("LoadKeys: %v", errLoad)
	}
	if len(keys) == 0 {
		t.Fatal("an empty prefix should not filter anything out")
	}
}

// realWorldConfig mirrors the provider block an opencode deployment actually
// uses: an underscore in the provider name, a base-url that already carries the
// /chat/completions path, per-key weights, and cooling left to the plugin.
// defaultPrefixForTest mirrors pluginconfig.DefaultBaseURLPrefix without making
// this package depend on the plugin config layer.
const defaultPrefixForTest = "https://opencode.ai"

const realWorldConfig = `
openai-compatibility:
  - name: opencode_go
    base-url: https://opencode.ai/zen/go/v1/chat/completions
    disable-cooling: true
    api-key-entries:
      - api-key: oc_sk_xxxx01
        weight: 1
      - api-key: oc_sk_xxxx02
        weight: 1
    models:
      - name: deepseek-v4.1-flash
        alias: ""
      - name: deepseek-v4-pro
        alias: ""
`

func TestLoadKeysHandlesTheRealWorldProviderShape(t *testing.T) {
	path := writeConfig(t, realWorldConfig)

	keys, errLoad := LoadKeys(path, "https://opencode.ai")
	if errLoad != nil {
		t.Fatalf("LoadKeys: %v", errLoad)
	}
	if len(keys) != 2 {
		t.Fatalf("got %d credentials, want 2", len(keys))
	}

	want := map[string]string{
		"oc_sk_xxxx01": "be6bf799f8f0",
		"oc_sk_xxxx02": "06ff50e27a06",
	}
	for _, key := range keys {
		// The underscore survives into both the provider key and the auth id.
		if key.Provider != "openai-compatible-opencode_go" {
			t.Errorf("provider = %q", key.Provider)
		}
		if key.Token != want[key.APIKey] {
			t.Errorf("token = %q for %s, want %q", key.Token, key.APIKey, want[key.APIKey])
		}
		if key.AuthID != "openai-compatibility:opencode_go:"+want[key.APIKey] {
			t.Errorf("auth id = %q for %s", key.AuthID, key.APIKey)
		}
		// The base-url is hashed verbatim. Normalising away the completions path
		// would break the match against the host's own candidate ids.
		if key.BaseURL != "https://opencode.ai/zen/go/v1/chat/completions" {
			t.Errorf("base url = %q", key.BaseURL)
		}
	}
}

func TestLoadKeysPrefixBoundariesWithAPathedBaseURL(t *testing.T) {
	path := writeConfig(t, realWorldConfig)

	// The configured base-url ends in /chat/completions, so the default prefix has
	// to match on the origin alone. Being stricter would silently empty the pool.
	keys, errLoad := LoadKeys(path, defaultPrefixForTest)
	if errLoad != nil {
		t.Fatalf("LoadKeys: %v", errLoad)
	}
	if len(keys) != 2 {
		t.Fatalf("the default prefix matched %d credentials, want 2", len(keys))
	}

	// A prefix that happens to equal the full base-url still matches.
	exact, _ := LoadKeys(path, "https://opencode.ai/zen/go/v1/chat/completions")
	if len(exact) != 2 {
		t.Errorf("an exact prefix matched %d credentials, want 2", len(exact))
	}

	// A different path segment does not.
	none, _ := LoadKeys(path, "https://opencode.ai/zen/go/v2")
	if len(none) != 0 {
		t.Errorf("an unrelated prefix matched %d credentials, want 0", len(none))
	}
}
