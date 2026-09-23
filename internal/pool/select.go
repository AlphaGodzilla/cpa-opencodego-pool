package pool

import (
	"sort"
	"strings"
	"time"

	"opencodego-pool/internal/keysource"
)

// Candidate is the subset of a scheduler candidate this plugin needs.
//
// Attributes are already redacted by the host: the plaintext api-key is never
// present. The credential is identified through the `source` attribute, which
// carries the same token the host hashed into the auth record ID.
type Candidate struct {
	ID         string
	Attributes map[string]string
}

// Input is one scheduler.pick request reduced to what selection needs.
type Input struct {
	Session    string
	Candidates []Candidate
}

// Settings carries the knobs a config reload can change. The caller snapshots
// them per pick so a concurrent reload cannot be observed halfway.
type Settings struct {
	BaseURLPrefix string
	StaleAfter    time.Duration
	ExcludeTokens map[string]struct{}
	PinTokens     map[string]string
}

// Outcome classifies the selector verdict, for stats and the resource page.
type Outcome string

const (
	OutcomeUnhandled     Outcome = "unhandled"
	OutcomeMappingFailed Outcome = "mapping_failed"
	OutcomePinned        Outcome = "pinned"
	OutcomeBound         Outcome = "bound"
	OutcomeAssigned      Outcome = "assigned"
	OutcomeRebound       Outcome = "rebound"
	OutcomeFallback      Outcome = "fallback"
)

// Result is the selector verdict.
type Result struct {
	Handled bool
	AuthID  string
	Token   string
	Outcome Outcome
	Reason  string
}

// Selector implements the routing policy. It holds no mutable configuration of
// its own; everything that can change lives in Settings.
type Selector struct {
	Registry *Registry
	Sessions *Sessions
	Now      func() time.Time
}

type pickable struct {
	state       State
	candidateID string
}

// Pick applies the routing policy and returns the credential to use.
//
// It never returns an error: any unexpected condition degrades to Handled=false
// so the host falls back to its built-in scheduler. Returning an error from
// scheduler.pick would fail the client request outright.
func (s *Selector) Pick(in Input, set Settings) Result {
	now := s.now()

	poolCandidates := make([]pickable, 0, len(in.Candidates))
	seen := make(map[string]struct{}, len(in.Candidates))
	matched := 0
	for _, candidate := range in.Candidates {
		if !keysource.MatchPrefix(candidate.Attributes["base_url"], set.BaseURLPrefix) {
			continue
		}
		token := keysource.TokenFromSource(candidate.Attributes["source"])
		if token == "" {
			continue
		}
		state, ok := s.Registry.Get(token)
		if !ok {
			// A candidate we cannot explain. It stays unselectable: inventing a
			// routing decision for an unknown credential is worse than deferring.
			continue
		}
		matched++
		if _, dup := seen[token]; !dup {
			seen[token] = struct{}{}
			s.Registry.Touch(token, now)
		}
		poolCandidates = append(poolCandidates, pickable{state: state, candidateID: candidate.ID})
	}

	if len(in.Candidates) > 0 && matched == 0 {
		// Nothing in this pick could be mapped back to a known credential while
		// the request clearly targeted the pool. Either the token recipe drifted
		// or config.yaml is out of sync; refuse rather than misroute.
		poolHasCandidate := false
		for _, candidate := range in.Candidates {
			if keysource.MatchPrefix(candidate.Attributes["base_url"], set.BaseURLPrefix) {
				poolHasCandidate = true
				break
			}
		}
		if poolHasCandidate {
			return Result{
				Outcome: OutcomeMappingFailed,
				Reason:  "candidates target the opencode pool but none matched a known credential",
			}
		}
		return Result{Outcome: OutcomeUnhandled, Reason: "no candidate belongs to the opencode pool"}
	}
	if len(poolCandidates) == 0 {
		return Result{Outcome: OutcomeUnhandled, Reason: "no candidate belongs to the opencode pool"}
	}

	session := strings.TrimSpace(in.Session)
	if session == "" {
		return Result{Outcome: OutcomeUnhandled, Reason: "request carries no session header"}
	}

	providers := make(map[string]struct{}, 2)
	for _, item := range poolCandidates {
		providers[item.state.Provider] = struct{}{}
	}
	if len(providers) != 1 {
		return Result{Outcome: OutcomeUnhandled, Reason: "candidates span more than one provider"}
	}
	provider := poolCandidates[0].state.Provider

	eligible := make([]pickable, 0, len(poolCandidates))
	for _, item := range poolCandidates {
		if s.ExclusionReason(item.state, set, now) != "" {
			continue
		}
		eligible = append(eligible, item)
	}

	// Every credential is unusable. Rather than failing the request outright,
	// release the least-exhausted one: a window may have reset since the last poll.
	if len(eligible) == 0 {
		chosen, ok := fallbackPick(poolCandidates, set)
		if !ok {
			return Result{Outcome: OutcomeUnhandled, Reason: "no credential available"}
		}
		s.Sessions.Bind(session, provider, chosen.state.Token)
		return Result{
			Handled: true,
			AuthID:  chosen.candidateID,
			Token:   chosen.state.Token,
			Outcome: OutcomeFallback,
			Reason:  "every credential is unusable; released the least exhausted one",
		}
	}

	// An explicit pin outranks both the sticky binding and the usage ranking.
	if pinnedToken, ok := set.PinTokens[session]; ok {
		for _, item := range eligible {
			if item.state.Token != pinnedToken {
				continue
			}
			s.Sessions.Bind(session, provider, item.state.Token)
			return Result{
				Handled: true,
				AuthID:  item.candidateID,
				Token:   item.state.Token,
				Outcome: OutcomePinned,
			}
		}
	}

	if boundToken, ok := s.Sessions.Get(session, provider); ok {
		for _, item := range eligible {
			if item.state.Token != boundToken {
				continue
			}
			return Result{
				Handled: true,
				AuthID:  item.candidateID,
				Token:   item.state.Token,
				Outcome: OutcomeBound,
			}
		}
		// The bound credential disappeared or became unusable: rebind right away.
		// Reassignment is still deterministic, so the "never route randomly" rule holds.
		chosen := rankEligible(eligible, set, now)[0]
		s.Sessions.Bind(session, provider, chosen.state.Token)
		return Result{
			Handled: true,
			AuthID:  chosen.candidateID,
			Token:   chosen.state.Token,
			Outcome: OutcomeRebound,
			Reason:  "previously bound credential is no longer usable",
		}
	}

	chosen := rankEligible(eligible, set, now)[0]
	s.Sessions.Bind(session, provider, chosen.state.Token)
	return Result{
		Handled: true,
		AuthID:  chosen.candidateID,
		Token:   chosen.state.Token,
		Outcome: OutcomeAssigned,
	}
}

// ExclusionReason explains why a credential would be skipped, or "" when it is usable.
func (s *Selector) ExclusionReason(state State, set Settings, now time.Time) string {
	if state.Revoked {
		if state.RevokedReason != "" {
			return "revoked: " + state.RevokedReason
		}
		return "revoked"
	}
	if unavailable, reason := state.Usage.Unavailable(); unavailable {
		return "exhausted: " + reason
	}
	if _, excluded := set.ExcludeTokens[state.Token]; excluded {
		return "excluded by config"
	}
	if state.Suspected429(now) {
		return "recent upstream 429"
	}
	return ""
}

// rankEligible orders usable credentials: rolling percent ascending, unmeasured
// or stale ones last, and the host candidate ID as a deterministic tie-break.
func rankEligible(items []pickable, set Settings, now time.Time) []pickable {
	out := make([]pickable, len(items))
	copy(out, items)
	sort.SliceStable(out, func(i, j int) bool {
		left, leftOK := rankPercent(out[i].state, set, now)
		right, rightOK := rankPercent(out[j].state, set, now)
		if leftOK != rightOK {
			return leftOK
		}
		if leftOK && left != right {
			return left < right
		}
		return out[i].candidateID < out[j].candidateID
	})
	return out
}

func rankPercent(state State, set Settings, now time.Time) (float64, bool) {
	if !state.HasUsage {
		return 0, false
	}
	if set.StaleAfter > 0 && now.Sub(state.FetchedAt) > set.StaleAfter {
		return 0, false
	}
	return state.Usage.SortPercent()
}

// fallbackPick chooses among credentials that the normal policy holds out,
// preferring the one furthest from its ceiling.
//
// Only permanently dead credentials are excluded here: a revoked key or one the
// operator explicitly excluded must never be released, even as a last resort.
// Everything else is fair game, including a key held out merely for a recent
// 429 — its measured usage may still show room.
func fallbackPick(items []pickable, set Settings) (pickable, bool) {
	releasable := make([]pickable, 0, len(items))
	for _, item := range items {
		if item.state.Revoked {
			continue
		}
		if _, excluded := set.ExcludeTokens[item.state.Token]; excluded {
			continue
		}
		releasable = append(releasable, item)
	}
	if len(releasable) == 0 {
		return pickable{}, false
	}

	best := -1
	bestMax := 0.0
	for i, item := range releasable {
		maxPercent, ok := item.state.Usage.MaxPercent()
		if !ok {
			continue
		}
		if best < 0 || maxPercent < bestMax ||
			(maxPercent == bestMax && item.candidateID < releasable[best].candidateID) {
			best, bestMax = i, maxPercent
		}
	}
	if best >= 0 {
		return releasable[best], true
	}

	// Nothing has ever been measured: a deterministic order still beats a
	// random one, and the next poll will replace this guess with real numbers.
	sorted := make([]pickable, len(releasable))
	copy(sorted, releasable)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].candidateID < sorted[j].candidateID })
	return sorted[0], true
}

func (s *Selector) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
