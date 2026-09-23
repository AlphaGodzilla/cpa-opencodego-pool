// Package keysource resolves the opencode api-keys that CLIProxyAPI synthesizes
// from `openai-compatibility` config entries, and reproduces the host's stable
// token recipe so those keys can be correlated with live scheduler candidates.
//
// The host never hands a scheduler plugin the plaintext key (candidate attributes
// are redacted), so this package is the only way to learn which credential a
// candidate refers to.
package keysource

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// hashLen is the number of hex characters the host keeps from the digest.
const hashLen = 12

// compatProviderPrefix mirrors internal/util.openAICompatibleProviderPrefix.
const compatProviderPrefix = "openai-compatible-"

// Generator reproduces CLIProxyAPI's internal/watcher/synthesizer.StableIDGenerator.
//
// The host builds one generator per synthesis pass and derives every config-backed
// auth ID from it, so a faithful reproduction must keep the same per-kind counters.
// Not safe for concurrent use.
type Generator struct {
	counters map[string]int
}

// NewGenerator returns an empty generator.
func NewGenerator() *Generator {
	return &Generator{counters: make(map[string]int)}
}

// Next reproduces StableIDGenerator.Next and returns the short token.
//
//	digest = sha256(kind || 0x00 || part...)      (each part trimmed)
//	token  = hex(digest)[:12], with "-N" appended for the Nth duplicate (kind, token).
func (g *Generator) Next(kind string, parts ...string) string {
	if g == nil {
		return strings.Repeat("0", hashLen)
	}
	hasher := sha256.New()
	hasher.Write([]byte(kind))
	for _, part := range parts {
		hasher.Write([]byte{0})
		hasher.Write([]byte(strings.TrimSpace(part)))
	}
	short := hex.EncodeToString(hasher.Sum(nil))
	if len(short) < hashLen {
		short = strings.Repeat("0", hashLen-len(short)) + short
	}
	short = short[:hashLen]

	key := kind + ":" + short
	index := g.counters[key]
	g.counters[key] = index + 1
	if index > 0 {
		short = fmt.Sprintf("%s-%d", short, index)
	}
	return short
}

// KindForProvider reproduces the host's idKind for an openai-compatibility provider.
func KindForProvider(name string) string {
	normalized := strings.ToLower(strings.TrimSpace(name))
	if normalized == "" {
		normalized = "openai-compatibility"
	}
	return "openai-compatibility:" + normalized
}

// ProviderKey reproduces internal/util.OpenAICompatibleProviderKey, which is what
// lands in the auth record's Provider field and in candidate attributes.
func ProviderKey(name string) string {
	normalized := strings.ToLower(strings.TrimSpace(name))
	if normalized == "" {
		return "openai-compatibility"
	}
	if normalized == "openai-compatibility" || strings.HasPrefix(normalized, compatProviderPrefix) {
		return normalized
	}
	return compatProviderPrefix + normalized
}

// AuthID joins a kind and token the way the host renders an auth record ID.
//
// This value is never used to select a credential directly — candidates are matched
// by token. It exists so the same identifiers can be surfaced in diagnostics.
func AuthID(kind, token string) string {
	return kind + ":" + token
}

// TokenFromSource extracts the token from the auth record `source` attribute,
// which the host renders as "config:<provider>[<token>]".
//
// The attribute key "source" survives the scheduler's sensitive-attribute filter,
// which is why it is usable here.
func TokenFromSource(source string) string {
	trimmed := strings.TrimSpace(source)
	open := strings.LastIndexByte(trimmed, '[')
	if open < 0 {
		return ""
	}
	closeIdx := strings.LastIndexByte(trimmed, ']')
	if closeIdx <= open {
		return ""
	}
	return strings.TrimSpace(trimmed[open+1 : closeIdx])
}
