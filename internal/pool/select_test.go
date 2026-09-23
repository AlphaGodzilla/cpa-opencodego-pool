package pool

import (
	"testing"
	"time"

	"opencodego-pool/internal/keysource"
	"opencodego-pool/internal/usage"
)

const (
	testPrefix   = "https://opencode.ai"
	testBaseURL  = "https://opencode.ai/zen/go/v1"
	testOtherURL = "https://api.example.com/v1"
	testProvider = "openai-compatible-opencodego"
)

var selectClock = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

type harness struct {
	selector *Selector
	registry *Registry
	sessions *Sessions
	settings Settings
}

func newHarness() *harness {
	now := func() time.Time { return selectClock }
	registry := NewRegistry(now)
	sessions := NewSessions(time.Hour, 100, now)
	return &harness{
		selector: &Selector{Registry: registry, Sessions: sessions, Now: now},
		registry: registry,
		sessions: sessions,
		settings: Settings{BaseURLPrefix: testPrefix, StaleAfter: 30 * time.Minute},
	}
}

// withTokens installs credentials for the opencode provider, one per token.
func (h *harness) withTokens(tokens ...string) {
	keys := make([]keysource.Key, 0, len(tokens))
	for _, token := range tokens {
		keys = append(keys, keysource.Key{
			Token:    token,
			Name:     "opencodego",
			Provider: testProvider,
			AuthID:   "openai-compatibility:opencodego:" + token,
			APIKey:   "sk-" + token,
			BaseURL:  testBaseURL,
		})
	}
	h.registry.Sync(keys)
}

func (h *harness) withKeys(keys ...keysource.Key) { h.registry.Sync(keys) }

func (h *harness) use(token string, rolling, weekly, monthly float64, weeklyStatus string) {
	h.useAt(token, rolling, weekly, monthly, weeklyStatus, selectClock)
}

func (h *harness) useAt(token string, rolling, weekly, monthly float64, weeklyStatus string, at time.Time) {
	h.registry.StoreUsage(token, usage.Usage{
		Rolling: usage.Window{Status: "ok", Percent: rolling, HasPercent: true},
		Weekly:  usage.Window{Status: weeklyStatus, Percent: weekly, HasPercent: true},
		Monthly: usage.Window{Status: "ok", Percent: monthly, HasPercent: true},
		Windows: 3,
	}, at)
}

func candidateFor(id, token string) Candidate {
	return Candidate{ID: id, Attributes: map[string]string{
		"base_url": testBaseURL,
		"source":   "config:opencodego[" + token + "]",
		"provider": testProvider,
	}}
}

func candidateAt(id, token, baseURL string) Candidate {
	return Candidate{ID: id, Attributes: map[string]string{
		"base_url": baseURL,
		"source":   "config:opencodego[" + token + "]",
	}}
}

func pickFor(h *harness, session string, candidates ...Candidate) Result {
	return h.selector.Pick(Input{Session: session, Candidates: candidates}, h.settings)
}

func TestPickAssignsTheLeastUsedCredential(t *testing.T) {
	h := newHarness()
	h.withTokens("a", "b", "c")
	h.use("a", 10, 0, 0, "ok")
	h.use("b", 50, 0, 0, "ok")
	h.use("c", 90, 0, 0, "ok")

	result := pickFor(h, "session-1", candidateFor("id-a", "a"), candidateFor("id-b", "b"), candidateFor("id-c", "c"))
	if !result.Handled || result.AuthID != "id-a" {
		t.Fatalf("result = %+v, want a handled pick of id-a", result)
	}
	if result.Outcome != OutcomeAssigned {
		t.Errorf("outcome = %q, want %q", result.Outcome, OutcomeAssigned)
	}
}

func TestPickIgnoresWeeklyAndMonthlyWhenRanking(t *testing.T) {
	h := newHarness()
	h.withTokens("a", "b")
	// Both rolling values are equal, so the ranking must fall through to the
	// deterministic tie-break rather than to the higher weekly value.
	h.use("a", 10, 90, 90, "ok")
	h.use("b", 10, 0, 0, "ok")

	result := pickFor(h, "session-1", candidateFor("id-a", "a"), candidateFor("id-b", "b"))
	if result.AuthID != "id-a" {
		t.Fatalf("auth = %q, want id-a (weekly must not rank)", result.AuthID)
	}
}

func TestPickKeepsASessionSticky(t *testing.T) {
	h := newHarness()
	h.withTokens("a", "b")
	h.use("a", 10, 0, 0, "ok")
	h.use("b", 90, 0, 0, "ok")

	first := pickFor(h, "session-1", candidateFor("id-a", "a"), candidateFor("id-b", "b"))
	if first.AuthID != "id-a" {
		t.Fatalf("first pick = %q, want id-a", first.AuthID)
	}

	// The other credential becomes the emptiest; the session must not move.
	h.use("b", 0, 0, 0, "ok")
	second := pickFor(h, "session-1", candidateFor("id-a", "a"), candidateFor("id-b", "b"))
	if second.AuthID != "id-a" {
		t.Fatalf("second pick = %q, want id-a (sticky)", second.AuthID)
	}
	if second.Outcome != OutcomeBound {
		t.Errorf("outcome = %q, want %q", second.Outcome, OutcomeBound)
	}
}

func TestPickSeparatesDistinctSessions(t *testing.T) {
	h := newHarness()
	h.withTokens("a", "b", "c")
	h.use("a", 0, 0, 0, "ok")
	h.use("b", 5, 0, 0, "ok")
	h.use("c", 10, 0, 0, "ok")

	candidates := []Candidate{candidateFor("id-a", "a"), candidateFor("id-b", "b"), candidateFor("id-c", "c")}
	if got := pickFor(h, "s1", candidates...).AuthID; got != "id-a" {
		t.Errorf("s1 = %q, want id-a", got)
	}
	if got := pickFor(h, "s2", candidates...).AuthID; got != "id-a" {
		t.Errorf("s2 = %q, want id-a (a new session takes the emptiest key)", got)
	}
	if count := h.sessions.CountToken("a"); count != 2 {
		t.Errorf("bindings on a = %d, want 2", count)
	}
}

func TestPickDeclinesWithoutASessionHeader(t *testing.T) {
	h := newHarness()
	h.withTokens("a")
	h.use("a", 0, 0, 0, "ok")

	result := pickFor(h, "   ", candidateFor("id-a", "a"))
	if result.Handled {
		t.Fatalf("result = %+v, want unhandled", result)
	}
	if result.Outcome != OutcomeUnhandled {
		t.Errorf("outcome = %q", result.Outcome)
	}
}

func TestPickDeclinesForeignProviders(t *testing.T) {
	h := newHarness()
	h.withTokens("a")
	h.use("a", 0, 0, 0, "ok")

	result := pickFor(h, "session-1", candidateAt("id-a", "a", testOtherURL))
	if result.Handled || result.Outcome != OutcomeUnhandled {
		t.Fatalf("result = %+v, want unhandled", result)
	}
}

func TestPickRefusesWhenCandidatesCannotBeMapped(t *testing.T) {
	h := newHarness()
	h.withTokens("a")
	h.use("a", 0, 0, 0, "ok")

	// The candidate belongs to the pool but its token is unknown: the recipe or
	// config.yaml has drifted, so routing must stop rather than guess.
	result := pickFor(h, "session-1", candidateFor("id-unknown", "nope"))
	if result.Handled {
		t.Fatalf("result = %+v, want unhandled", result)
	}
	if result.Outcome != OutcomeMappingFailed {
		t.Fatalf("outcome = %q, want %q", result.Outcome, OutcomeMappingFailed)
	}
}

func TestPickSkipsExhaustedCredentials(t *testing.T) {
	h := newHarness()
	h.withTokens("a", "b")
	h.use("a", 0, 100, 0, "rate-limited")
	h.use("b", 50, 0, 0, "ok")

	result := pickFor(h, "session-1", candidateFor("id-a", "a"), candidateFor("id-b", "b"))
	if result.AuthID != "id-b" {
		t.Fatalf("auth = %q, want id-b (a is rate limited on a weekly window)", result.AuthID)
	}
}

func TestPickRebindsWhenTheBoundCredentialGoesBad(t *testing.T) {
	h := newHarness()
	h.withTokens("a", "b")
	h.use("a", 0, 0, 0, "ok")
	h.use("b", 50, 0, 0, "ok")

	candidates := []Candidate{candidateFor("id-a", "a"), candidateFor("id-b", "b")}
	if got := pickFor(h, "session-1", candidates...).AuthID; got != "id-a" {
		t.Fatalf("initial pick = %q, want id-a", got)
	}

	h.use("a", 100, 100, 0, "rate-limited")
	result := pickFor(h, "session-1", candidates...)
	if result.AuthID != "id-b" {
		t.Fatalf("auth = %q, want id-b after the bound key ran out", result.AuthID)
	}
	if result.Outcome != OutcomeRebound {
		t.Errorf("outcome = %q, want %q", result.Outcome, OutcomeRebound)
	}
	if count := h.sessions.CountToken("a"); count != 0 {
		t.Errorf("the old binding should have been replaced, found %d", count)
	}
}

func TestPickRebindsWhenTheBoundCredentialDisappears(t *testing.T) {
	h := newHarness()
	h.withTokens("a", "b")
	h.use("a", 0, 0, 0, "ok")
	h.use("b", 50, 0, 0, "ok")

	if got := pickFor(h, "session-1", candidateFor("id-a", "a"), candidateFor("id-b", "b")).AuthID; got != "id-a" {
		t.Fatalf("initial pick = %q", got)
	}

	// The key is removed from config: it stops appearing as a candidate.
	result := pickFor(h, "session-1", candidateFor("id-b", "b"))
	if result.AuthID != "id-b" || result.Outcome != OutcomeRebound {
		t.Fatalf("result = %+v, want a rebound to id-b", result)
	}
}

func TestPickFallsBackWhenEveryCredentialIsUnusable(t *testing.T) {
	h := newHarness()
	h.withTokens("a", "b")
	// Both are gated off, but b is further from its ceiling.
	h.use("a", 100, 100, 5, "rate-limited")
	h.use("b", 95, 40, 40, "ok")
	// Both credentials are held out of the candidate set, so the selector has to
	// release the least exhausted one instead of failing the request.
	h.registry.MarkSuspected429("a", selectClock)
	h.registry.MarkSuspected429("b", selectClock)

	result := pickFor(h, "session-1", candidateFor("id-a", "a"), candidateFor("id-b", "b"))
	if !result.Handled {
		t.Fatalf("result = %+v, want a handled fallback", result)
	}
	if result.Outcome != OutcomeFallback {
		t.Fatalf("outcome = %q, want %q", result.Outcome, OutcomeFallback)
	}
	if result.AuthID != "id-b" {
		t.Fatalf("auth = %q, want id-b (lowest ceiling)", result.AuthID)
	}
}

func TestPickFallbackNeverReleasesRevokedCredentials(t *testing.T) {
	h := newHarness()
	h.withTokens("a", "b")
	h.use("a", 10, 0, 0, "ok")
	h.use("b", 100, 0, 0, "rate-limited")
	h.registry.StoreRevoked("a", "usage endpoint returned 401")

	result := pickFor(h, "session-1", candidateFor("id-a", "a"), candidateFor("id-b", "b"))
	if result.AuthID != "id-b" {
		t.Fatalf("auth = %q, want id-b (the revoked key must stay out)", result.AuthID)
	}
}

func TestPickFallsBackDeterministicallyWhenNothingIsMeasured(t *testing.T) {
	h := newHarness()
	h.withTokens("a", "b")
	h.registry.MarkSuspected429("a", selectClock)
	h.registry.MarkSuspected429("b", selectClock)

	result := pickFor(h, "session-1", candidateFor("id-b", "b"), candidateFor("id-a", "a"))
	if result.Outcome != OutcomeFallback || result.AuthID != "id-a" {
		t.Fatalf("result = %+v, want a fallback to id-a", result)
	}
}

func TestPickHonoursAnExplicitPin(t *testing.T) {
	h := newHarness()
	h.withTokens("a", "b")
	h.use("a", 0, 0, 0, "ok")
	h.use("b", 90, 0, 0, "ok")
	h.settings.PinTokens = map[string]string{"session-1": "b"}

	result := pickFor(h, "session-1", candidateFor("id-a", "a"), candidateFor("id-b", "b"))
	if result.AuthID != "id-b" || result.Outcome != OutcomePinned {
		t.Fatalf("result = %+v, want a pinned pick of id-b", result)
	}
}

func TestPickIgnoresAPinThatIsNoLongerUsable(t *testing.T) {
	h := newHarness()
	h.withTokens("a", "b")
	h.use("a", 0, 0, 0, "ok")
	h.use("b", 100, 100, 100, "rate-limited")
	h.settings.PinTokens = map[string]string{"session-1": "b"}

	result := pickFor(h, "session-1", candidateFor("id-a", "a"), candidateFor("id-b", "b"))
	if result.AuthID != "id-a" || result.Outcome != OutcomeAssigned {
		t.Fatalf("result = %+v, want an ordinary assignment to id-a", result)
	}
}

func TestPickHonoursExcludeKeys(t *testing.T) {
	h := newHarness()
	h.withTokens("a", "b")
	h.use("a", 0, 0, 0, "ok")
	h.use("b", 90, 0, 0, "ok")
	h.settings.ExcludeTokens = map[string]struct{}{"a": {}}

	result := pickFor(h, "session-1", candidateFor("id-a", "a"), candidateFor("id-b", "b"))
	if result.AuthID != "id-b" {
		t.Fatalf("auth = %q, want id-b (a is excluded by config)", result.AuthID)
	}
}

func TestPickRanksUnmeasuredCredentialsLast(t *testing.T) {
	h := newHarness()
	h.withTokens("a", "b")
	// Only b has been measured; a must not jump ahead of it.
	h.use("b", 70, 0, 0, "ok")

	result := pickFor(h, "session-1", candidateFor("id-a", "a"), candidateFor("id-b", "b"))
	if result.AuthID != "id-b" {
		t.Fatalf("auth = %q, want id-b (unmeasured sorts last)", result.AuthID)
	}
}

func TestPickRanksStaleCredentialsLast(t *testing.T) {
	h := newHarness()
	h.withTokens("a", "b")
	h.useAt("a", 0, 0, 0, "ok", selectClock.Add(-2*time.Hour))
	h.use("b", 80, 0, 0, "ok")

	result := pickFor(h, "session-1", candidateFor("id-a", "a"), candidateFor("id-b", "b"))
	if result.AuthID != "id-b" {
		t.Fatalf("auth = %q, want id-b (stale data must not rank)", result.AuthID)
	}
}

func TestPickTieBreakIsDeterministic(t *testing.T) {
	h := newHarness()
	h.withTokens("a", "b")
	h.use("a", 42, 0, 0, "ok")
	h.use("b", 42, 0, 0, "ok")

	// The candidate order is reversed on purpose: the tie-break must not depend on it.
	result := pickFor(h, "session-1", candidateFor("id-b", "b"), candidateFor("id-a", "a"))
	if result.AuthID != "id-a" {
		t.Fatalf("auth = %q, want id-a (lowest candidate id wins a tie)", result.AuthID)
	}
}

func TestPickDeclinesWhenCandidatesSpanProviders(t *testing.T) {
	h := newHarness()
	h.withKeys(
		keysource.Key{Token: "a", Provider: "openai-compatible-opencodego", APIKey: "sk-a", BaseURL: testBaseURL, AuthID: "k:a"},
		keysource.Key{Token: "b", Provider: "openai-compatible-other", APIKey: "sk-b", BaseURL: testBaseURL, AuthID: "k:b"},
	)
	h.use("a", 0, 0, 0, "ok")
	h.use("b", 0, 0, 0, "ok")

	result := pickFor(h, "session-1", candidateFor("id-a", "a"), candidateFor("id-b", "b"))
	if result.Handled || result.Outcome != OutcomeUnhandled {
		t.Fatalf("result = %+v, want unhandled for a mixed-provider candidate set", result)
	}
}

func TestPickIgnoresCandidatesWithoutASourceAttribute(t *testing.T) {
	h := newHarness()
	h.withTokens("a")
	h.use("a", 0, 0, 0, "ok")

	broken := Candidate{ID: "id-a", Attributes: map[string]string{"base_url": testBaseURL}}
	result := pickFor(h, "session-1", broken)
	if result.Handled || result.Outcome != OutcomeMappingFailed {
		t.Fatalf("result = %+v, want a mapping failure", result)
	}
}

func TestPickTouchesCandidateTimestamps(t *testing.T) {
	h := newHarness()
	h.withTokens("a")
	h.use("a", 0, 0, 0, "ok")

	if _, ok := h.registry.Get("a"); !ok {
		t.Fatal("credential missing")
	}
	before, _ := h.registry.Get("a")
	if !before.LastSeenAt.IsZero() {
		t.Fatal("LastSeenAt should start zero")
	}

	pickFor(h, "session-1", candidateFor("id-a", "a"))

	after, _ := h.registry.Get("a")
	if !after.LastSeenAt.Equal(selectClock) {
		t.Fatalf("LastSeenAt = %v, want %v", after.LastSeenAt, selectClock)
	}
}

func TestExclusionReasonExplainsEachCause(t *testing.T) {
	h := newHarness()
	h.withTokens("a", "b", "c", "d", "e")

	h.use("a", 0, 0, 0, "ok")
	if reason := h.selector.ExclusionReason(mustState(t, h, "a"), h.settings, selectClock); reason != "" {
		t.Errorf("usable credential reported %q", reason)
	}

	h.use("b", 0, 100, 0, "rate-limited")
	if reason := h.selector.ExclusionReason(mustState(t, h, "b"), h.settings, selectClock); reason != "exhausted: weekly=rate-limited" {
		t.Errorf("exhausted reason = %q", reason)
	}

	h.use("c", 0, 0, 0, "ok")
	h.registry.StoreRevoked("c", "usage endpoint returned 401")
	if reason := h.selector.ExclusionReason(mustState(t, h, "c"), h.settings, selectClock); reason != "revoked: usage endpoint returned 401" {
		t.Errorf("revoked reason = %q", reason)
	}

	h.use("d", 0, 0, 0, "ok")
	h.settings.ExcludeTokens = map[string]struct{}{"d": {}}
	if reason := h.selector.ExclusionReason(mustState(t, h, "d"), h.settings, selectClock); reason != "excluded by config" {
		t.Errorf("excluded reason = %q", reason)
	}

	h.use("e", 0, 0, 0, "ok")
	h.registry.MarkSuspected429("e", selectClock)
	if reason := h.selector.ExclusionReason(mustState(t, h, "e"), h.settings, selectClock); reason != "recent upstream 429" {
		t.Errorf("429 reason = %q", reason)
	}

	// The 429 hold is a floor, not a permanent mark: once it elapses the credential
	// is judged on fresh usage data again.
	if reason := h.selector.ExclusionReason(mustState(t, h, "e"), h.settings, selectClock.Add(SuspectHold+time.Second)); reason != "" {
		t.Errorf("the 429 hold should have expired, got %q", reason)
	}
}

func mustState(t *testing.T, h *harness, token string) State {
	t.Helper()
	state, ok := h.registry.Get(token)
	if !ok {
		t.Fatalf("credential %s missing", token)
	}
	return state
}

func TestStoreUsageRespectsThe429Hold(t *testing.T) {
	clock := newTestClock()
	registry := NewRegistry(clock.now)
	registry.Sync([]keysource.Key{{
		Token: "a", Provider: testProvider, APIKey: "sk-hidden", BaseURL: testBaseURL,
	}})
	registry.MarkSuspected429("a", clock.current)

	parsed := usage.Usage{
		Rolling: usage.Window{Status: "ok", Percent: 5, HasPercent: true},
		Windows: 1,
	}

	// A read inside the hold leaves the suspicion in place, which is the whole point:
	// the usage endpoint can still be reporting a healthy credential.
	registry.StoreUsage("a", parsed, clock.current.Add(SuspectHold-time.Second))
	if state, _ := registry.Get("a"); state.Suspected429At.IsZero() {
		t.Error("a read inside the hold must not clear the suspicion")
	}

	// Past the hold, fresh data wins.
	registry.StoreUsage("a", parsed, clock.current.Add(SuspectHold+time.Second))
	if state, _ := registry.Get("a"); !state.Suspected429At.IsZero() {
		t.Error("a read past the hold should clear the suspicion")
	}
}

func TestRegistrySyncPreservesUsageForSurvivingTokens(t *testing.T) {
	h := newHarness()
	h.withTokens("a", "b")
	h.use("a", 10, 0, 0, "ok")

	added, removed := h.registry.Sync([]keysource.Key{
		{Token: "b", Provider: testProvider, APIKey: "sk-b", BaseURL: testBaseURL},
		{Token: "c", Provider: testProvider, APIKey: "sk-c", BaseURL: testBaseURL},
	})
	if len(added) != 1 || added[0] != "c" {
		t.Errorf("added = %v, want [c]", added)
	}
	if len(removed) != 1 || removed[0] != "a" {
		t.Errorf("removed = %v, want [a]", removed)
	}

	if _, ok := h.registry.Get("a"); ok {
		t.Error("a should be gone")
	}
	state, ok := h.registry.Get("b")
	if !ok {
		t.Fatal("b should have survived")
	}
	if state.HasUsage {
		t.Error("b never had usage, so it must not appear measured")
	}
}
