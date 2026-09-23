package keysource

import "testing"

// The expected tokens below were computed independently of this package with
// python hashlib, following the host's recipe:
//
//	sha256(kind || 0x00 || trim(key) || 0x00 || trim(base) || 0x00 || trim(proxy))[:12]
//
// Hard-coding them is the point: if the recipe ever drifts, these fail loudly
// instead of the plugin silently routing on a stale mapping.
const testBaseURL = "https://opencode.ai/zen/go/v1"

func TestKindForProvider(t *testing.T) {
	cases := map[string]string{
		"opencodeGo": "openai-compatibility:opencodego",
		"  OC  ":     "openai-compatibility:oc",
		"":           "openai-compatibility:openai-compatibility",
		"Mixed":      "openai-compatibility:mixed",
	}
	for input, want := range cases {
		if got := KindForProvider(input); got != want {
			t.Errorf("KindForProvider(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestGeneratorMatchesHostRecipe(t *testing.T) {
	kind := KindForProvider("opencodeGo")
	if kind != "openai-compatibility:opencodego" {
		t.Fatalf("unexpected kind %q", kind)
	}

	gen := NewGenerator()
	cases := []struct {
		name  string
		kind  string
		parts []string
		want  string
	}{
		{"first key", kind, []string{"sk-test-1", testBaseURL, ""}, "c7b6e31b521d"},
		{"second key", kind, []string{"sk-test-2", testBaseURL, ""}, "4c99b1e57635"},
		{"third key", kind, []string{"sk-test-3", testBaseURL, ""}, "b897025fa5d8"},
		{"other provider", "openai-compatibility:other", []string{"sk-test-1", "https://api.example.com/v1", ""}, "5e5626859f84"},
		{"per-key proxy", kind, []string{"sk-test-1", testBaseURL, "http://127.0.0.1:7890"}, "a49fc675ab9e"},
	}
	for _, testCase := range cases {
		if got := gen.Next(testCase.kind, testCase.parts...); got != testCase.want {
			t.Errorf("%s: token = %q, want %q", testCase.name, got, testCase.want)
		}
	}
}

func TestGeneratorTrimsPartsLikeTheHost(t *testing.T) {
	kind := KindForProvider("opencodeGo")

	plain := NewGenerator().Next(kind, "sk-spaced", testBaseURL, "")
	spaced := NewGenerator().Next(kind, "  sk-spaced  ", "  "+testBaseURL+"  ", "  ")
	if plain != spaced {
		t.Fatalf("whitespace changed the token: %q vs %q", plain, spaced)
	}
	// 59abf80e3b92 is sha256(kind \0 "sk-spaced" \0 base \0 "")[:12], computed
	// independently. It differs from the c7b6e31b521d token on purpose: this case
	// only proves that whitespace is trimmed before hashing.
	if plain != "59abf80e3b92" {
		t.Fatalf("unexpected token %q", plain)
	}
}

func TestGeneratorDuplicateSuffix(t *testing.T) {
	kind := KindForProvider("opencodeGo")
	gen := NewGenerator()

	first := gen.Next(kind, "sk-dup", testBaseURL, "")
	second := gen.Next(kind, "sk-dup", testBaseURL, "")
	third := gen.Next(kind, "sk-dup", testBaseURL, "")

	if second != first+"-1" {
		t.Errorf("second duplicate = %q, want %q", second, first+"-1")
	}
	if third != first+"-2" {
		t.Errorf("third duplicate = %q, want %q", third, first+"-2")
	}

	// A different kind keeps its own counter.
	other := NewGenerator().Next(KindForProvider("someoneelse"), "sk-dup", testBaseURL, "")
	if other == first {
		t.Fatalf("different providers must not share a counter")
	}
}

func TestProviderKey(t *testing.T) {
	cases := map[string]string{
		"opencodego":            "openai-compatible-opencodego",
		"  OpenCodeGo  ":        "openai-compatible-opencodego",
		"":                      "openai-compatibility",
		"openai-compatibility":  "openai-compatibility",
		"openai-compatible-abc": "openai-compatible-abc",
	}
	for input, want := range cases {
		if got := ProviderKey(input); got != want {
			t.Errorf("ProviderKey(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestAuthID(t *testing.T) {
	if got := AuthID("openai-compatibility:opencodego", "c7b6e31b521d"); got != "openai-compatibility:opencodego:c7b6e31b521d" {
		t.Fatalf("unexpected auth id %q", got)
	}
}

func TestTokenFromSource(t *testing.T) {
	cases := map[string]string{
		"config:opencodego[c7b6e31b521d]":     "c7b6e31b521d",
		"  config:opencodego[c7b6e31b521d]  ": "c7b6e31b521d",
		"config:opencodego[abc-1]":            "abc-1",
		"c7b6e31b521d":                        "",
		"config:opencodego":                   "",
		"config:opencodego[]":                 "",
		"config:opencodego[unclosed":          "",
		"":                                    "",
	}
	for input, want := range cases {
		if got := TokenFromSource(input); got != want {
			t.Errorf("TokenFromSource(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestMatchPrefix(t *testing.T) {
	const prefix = "https://opencode.ai"
	cases := map[string]bool{
		"https://opencode.ai":                        true,
		"https://opencode.ai/zen/go/v1":              true,
		"HTTPS://OPENCODE.AI/zen/go/v1":              true,
		"  https://opencode.ai/zen/go/v1  ":          true,
		"https://opencode.ai.evil.example/zen/go/v1": true,
		"https://api.example.com/v1":                 false,
		"":                                           false,
	}
	for input, want := range cases {
		if got := MatchPrefix(input, prefix); got != want {
			t.Errorf("MatchPrefix(%q) = %v, want %v", input, got, want)
		}
	}
	if MatchPrefix("https://opencode.ai", "") {
		t.Error("an empty prefix must never match")
	}
}
