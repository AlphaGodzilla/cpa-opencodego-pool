// Package pool holds the in-memory credential table, the session→credential
// bindings, and the selection algorithm. Everything here is pure logic: no host
// calls, no I/O, no clocks other than the injected ones.
package pool

import (
	"sync"
	"time"

	"opencodego-pool/internal/keysource"
	"opencodego-pool/internal/usage"
)

// State is everything the plugin knows about one credential.
//
// Tokens are the identity. The host assigns auth IDs by hashing the same inputs,
// but candidates are matched by the token embedded in their `source` attribute,
// so the plugin never has to reconstruct an auth ID to recognize a credential.
type State struct {
	Token    string
	Name     string
	Provider string
	AuthID   string
	APIKey   string
	BaseURL  string
	ProxyURL string

	// Usage is the last successfully fetched usage payload.
	Usage     usage.Usage
	HasUsage  bool
	FetchedAt time.Time

	// FailStreak counts consecutive failed usage fetches.
	FailStreak int
	// Revoked is set when the usage endpoint answers 401/403, which means the
	// key itself is dead rather than merely rate limited.
	Revoked       bool
	RevokedReason string

	// Suspected429At is set when a live request through this credential failed
	// with 429; it is cleared by the next successful usage fetch.
	Suspected429At time.Time

	// LastSeenAt is the last time this credential appeared in a candidate list.
	LastSeenAt time.Time
}

// SuspectHold is how long a live 429 keeps a credential out of the rotation.
//
// A 429 observed on the request path is hard evidence about right now, while the
// usage percentages are a soft signal that can lag behind it. Without a floor, the
// usage read that usage.handle immediately triggers would erase the 429 evidence
// whenever the endpoint still reported the credential as healthy, and the fast
// path would never actually protect anything.
//
// It matches pluginconfig.DefaultSuspectThrottle by design: the hold expires at
// roughly the moment the next out-of-band read is allowed to run, so the
// credential is re-evaluated against fresh data as soon as the hold lifts.
const SuspectHold = 60 * time.Second

// Suspected429 reports whether a live 429 is still within its hold window.
func (s State) Suspected429(now time.Time) bool {
	return !s.Suspected429At.IsZero() && now.Sub(s.Suspected429At) < SuspectHold
}

// Registry is the in-memory credential table.
type Registry struct {
	mu      sync.RWMutex
	keys    map[string]*State
	order   []string
	nowFunc func() time.Time
}

// NewRegistry creates an empty registry.
func NewRegistry(nowFunc func() time.Time) *Registry {
	if nowFunc == nil {
		nowFunc = time.Now
	}
	return &Registry{
		keys:    make(map[string]*State),
		nowFunc: nowFunc,
	}
}

// Sync replaces the credential table with the given config-derived keys,
// preserving accumulated usage state for credentials that survived the change.
//
// It returns the tokens that appeared and the tokens that disappeared so the
// caller can kick off an immediate refresh for new keys and drop the bindings of
// removed ones.
func (r *Registry) Sync(keys []keysource.Key) (added, removed []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	next := make(map[string]*State, len(keys))
	order := make([]string, 0, len(keys))
	for _, key := range keys {
		if _, dup := next[key.Token]; dup {
			continue
		}
		if existing, ok := r.keys[key.Token]; ok {
			existing.Name = key.Name
			existing.Provider = key.Provider
			existing.AuthID = key.AuthID
			existing.APIKey = key.APIKey
			existing.BaseURL = key.BaseURL
			existing.ProxyURL = key.ProxyURL
			next[key.Token] = existing
		} else {
			next[key.Token] = &State{
				Token:    key.Token,
				Name:     key.Name,
				Provider: key.Provider,
				AuthID:   key.AuthID,
				APIKey:   key.APIKey,
				BaseURL:  key.BaseURL,
				ProxyURL: key.ProxyURL,
			}
			added = append(added, key.Token)
		}
		order = append(order, key.Token)
	}
	for token := range r.keys {
		if _, ok := next[token]; !ok {
			removed = append(removed, token)
		}
	}

	r.keys = next
	r.order = order
	return added, removed
}

// Get returns a copy of the credential state, so callers never hold a live pointer.
func (r *Registry) Get(token string) (State, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	state, ok := r.keys[token]
	if !ok {
		return State{}, false
	}
	return *state, true
}

// Len reports how many credentials are tracked.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.keys)
}

// Snapshot returns copies of every credential in config order.
func (r *Registry) Snapshot() []State {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]State, 0, len(r.order))
	for _, token := range r.order {
		if state, ok := r.keys[token]; ok {
			out = append(out, *state)
		}
	}
	return out
}

// Touch records that a credential was offered as a candidate.
func (r *Registry) Touch(token string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if state, ok := r.keys[token]; ok {
		state.LastSeenAt = at
	}
}

// StoreUsage records a successful usage fetch, clearing failure and suspicion flags.
func (r *Registry) StoreUsage(token string, parsed usage.Usage, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state, ok := r.keys[token]
	if !ok {
		return
	}
	state.Usage = parsed
	state.HasUsage = true
	state.FetchedAt = at
	state.FailStreak = 0
	state.Revoked = false
	state.RevokedReason = ""
	// A successful read only clears the suspicion once the hold has elapsed.
	// Clearing it earlier would let a lagging usage endpoint undo the 429 evidence.
	if state.Suspected429At.IsZero() || at.Sub(state.Suspected429At) >= SuspectHold {
		state.Suspected429At = time.Time{}
	}
}

// StoreFetchFailure records an inconclusive fetch (network error, timeout, 5xx).
func (r *Registry) StoreFetchFailure(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if state, ok := r.keys[token]; ok {
		state.FailStreak++
	}
}

// StoreRevoked marks a credential whose usage endpoint answered 401/403.
func (r *Registry) StoreRevoked(token, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state, ok := r.keys[token]
	if !ok {
		return
	}
	state.Revoked = true
	state.RevokedReason = reason
	state.FailStreak = 0
	state.HasUsage = false
	state.Usage = usage.Usage{}
}

// MarkSuspected429 flags a credential that just failed a live request with 429.
func (r *Registry) MarkSuspected429(token string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if state, ok := r.keys[token]; ok {
		state.Suspected429At = at
	}
}

// IsTracked reports whether the token belongs to the current credential table.
func (r *Registry) IsTracked(token string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.keys[token]
	return ok
}

// Remove drops a credential and its accumulated state.
func (r *Registry) Remove(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.keys[token]; !ok {
		return
	}
	delete(r.keys, token)
	for i, existing := range r.order {
		if existing == token {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
}
