package keysource

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileConfig is the minimal slice of CLIProxyAPI's config.yaml that this plugin reads.
type FileConfig struct {
	OpenAICompatibility []FileCompat `yaml:"openai-compatibility"`
}

// FileCompat mirrors the host's openai-compatibility provider block.
type FileCompat struct {
	Name          string      `yaml:"name"`
	BaseURL       string      `yaml:"base-url"`
	Disabled      bool        `yaml:"disabled"`
	APIKeyEntries []FileEntry `yaml:"api-key-entries"`
}

// FileEntry mirrors one api-key-entry.
type FileEntry struct {
	APIKey   string `yaml:"api-key"`
	ProxyURL string `yaml:"proxy-url"`
}

// Key is one resolved api-key entry belonging to a matching opencode provider.
type Key struct {
	Token    string
	Name     string
	Provider string // provider key, e.g. openai-compatible-opencodego
	AuthID   string // cosmetic: the ID the host assigns to this credential
	APIKey   string
	BaseURL  string
	ProxyURL string
}

// LoadKeys reads configPath and returns every api-key-entry of an enabled
// openai-compatibility provider whose base-url starts with baseURLPrefix.
//
// The generator is advanced for every entry the host would synthesize, including
// entries with an empty api-key and providers filtered out by the base-url match,
// so duplicate-token suffixes stay aligned with the host's own counter. Only
// providers that match the prefix are reported — for those, the host's kind
// (which embeds the provider name) is unaffected by other providers' entries.
func LoadKeys(configPath, baseURLPrefix string) ([]Key, error) {
	raw, errRead := os.ReadFile(configPath)
	if errRead != nil {
		return nil, errRead
	}
	var cfg FileConfig
	if errUnmarshal := yaml.Unmarshal(raw, &cfg); errUnmarshal != nil {
		return nil, fmt.Errorf("parse %s: %w", configPath, errUnmarshal)
	}

	prefix := strings.ToLower(strings.TrimSpace(baseURLPrefix))
	gen := NewGenerator()
	out := make([]Key, 0, len(cfg.OpenAICompatibility))

	for _, compat := range cfg.OpenAICompatibility {
		if compat.Disabled {
			continue
		}
		base := strings.TrimSpace(compat.BaseURL)
		matches := prefix == "" || strings.HasPrefix(strings.ToLower(base), prefix)
		kind := KindForProvider(compat.Name)
		providerKey := ProviderKey(compat.Name)

		for _, entry := range compat.APIKeyEntries {
			apiKey := strings.TrimSpace(entry.APIKey)
			proxyURL := strings.TrimSpace(entry.ProxyURL)
			// Always advance the generator, even for entries we will not report,
			// so the host's duplicate-counter suffixes stay reproducible.
			token := gen.Next(kind, apiKey, base, proxyURL)
			if !matches || apiKey == "" {
				continue
			}
			out = append(out, Key{
				Token:    token,
				Name:     compat.Name,
				Provider: providerKey,
				AuthID:   AuthID(kind, token),
				APIKey:   apiKey,
				BaseURL:  base,
				ProxyURL: proxyURL,
			})
		}
	}
	return out, nil
}

// MatchPrefix reports whether baseURL belongs to the opencode pool.
func MatchPrefix(baseURL, prefix string) bool {
	normalizedPrefix := strings.ToLower(strings.TrimSpace(prefix))
	if normalizedPrefix == "" {
		return false
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(baseURL)), normalizedPrefix)
}
