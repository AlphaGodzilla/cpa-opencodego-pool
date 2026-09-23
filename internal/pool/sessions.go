package pool

import (
	"container/list"
	"sync"
	"time"
)

// Sessions maps (sessionID, provider) to a credential token with a sliding TTL
// and an LRU capacity bound.
//
// The table is pure memory by design: the spec accepts that a restart reassigns
// sessions rather than persisting bindings.
type Sessions struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	now   func() time.Time
	items map[string]*list.Element
	order *list.List
}

type bindingEntry struct {
	key    string
	token  string
	expiry time.Time
}

// NewSessions creates a binding table. A non-positive max disables the bound.
func NewSessions(ttl time.Duration, max int, nowFunc func() time.Time) *Sessions {
	if nowFunc == nil {
		nowFunc = time.Now
	}
	return &Sessions{
		ttl:   ttl,
		max:   max,
		now:   nowFunc,
		items: make(map[string]*list.Element),
		order: list.New(),
	}
}

// Configure updates the TTL and capacity bound in place. Reconfiguring rather
// than replacing the table keeps the selector's pointer stable, which matters
// because picks run concurrently with config reloads.
func (s *Sessions) Configure(ttl time.Duration, max int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ttl > 0 {
		s.ttl = ttl
	}
	if max > 0 {
		s.max = max
	}
	for s.max > 0 && s.order.Len() > s.max {
		oldest := s.order.Back()
		if oldest == nil {
			break
		}
		s.removeElement(oldest)
	}
}

func compositeKey(session, provider string) string {
	return session + "\x00" + provider
}

// Get returns the bound token for a session and renews both its TTL and its
// recency. Expired bindings are dropped on access.
func (s *Sessions) Get(session, provider string) (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	key := compositeKey(session, provider)
	element, ok := s.items[key]
	if !ok {
		return "", false
	}
	entry := element.Value.(*bindingEntry)
	now := s.now()
	if !entry.expiry.IsZero() && !now.Before(entry.expiry) {
		s.removeElement(element)
		return "", false
	}
	entry.expiry = now.Add(s.ttl)
	s.order.MoveToFront(element)
	return entry.token, true
}

// Token returns the bound token without renewing it.
func (s *Sessions) Token(session, provider string) (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	element, ok := s.items[compositeKey(session, provider)]
	if !ok {
		return "", false
	}
	entry := element.Value.(*bindingEntry)
	if !entry.expiry.IsZero() && !s.now().Before(entry.expiry) {
		return "", false
	}
	return entry.token, true
}

// Bind creates or refreshes a binding and evicts the least recently used entries
// beyond the capacity bound.
func (s *Sessions) Bind(session, provider, token string) {
	if s == nil || token == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	key := compositeKey(session, provider)
	now := s.now()
	if element, ok := s.items[key]; ok {
		entry := element.Value.(*bindingEntry)
		entry.token = token
		entry.expiry = now.Add(s.ttl)
		s.order.MoveToFront(element)
		return
	}
	entry := &bindingEntry{key: key, token: token, expiry: now.Add(s.ttl)}
	s.items[key] = s.order.PushFront(entry)
	for s.max > 0 && s.order.Len() > s.max {
		oldest := s.order.Back()
		if oldest == nil {
			break
		}
		s.removeElement(oldest)
	}
}

func (s *Sessions) removeElement(element *list.Element) {
	if element == nil {
		return
	}
	entry, ok := element.Value.(*bindingEntry)
	if ok {
		delete(s.items, entry.key)
	}
	s.order.Remove(element)
}

// DropToken removes every binding that points at the given credential and returns
// how many were dropped. Used when a key disappears from config.
func (s *Sessions) DropToken(token string) int {
	if s == nil || token == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	dropped := 0
	for element := s.order.Front(); element != nil; {
		next := element.Next()
		if entry, ok := element.Value.(*bindingEntry); ok && entry.token == token {
			s.removeElement(element)
			dropped++
		}
		element = next
	}
	return dropped
}

// CountToken returns how many sessions are bound to a credential.
func (s *Sessions) CountToken(token string) int {
	if s == nil || token == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	for element := s.order.Front(); element != nil; element = element.Next() {
		if entry, ok := element.Value.(*bindingEntry); ok && entry.token == token {
			count++
		}
	}
	return count
}

// Sweep drops expired bindings and returns how many were removed.
func (s *Sessions) Sweep() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	removed := 0
	for element := s.order.Front(); element != nil; {
		next := element.Next()
		if entry, ok := element.Value.(*bindingEntry); ok {
			if !entry.expiry.IsZero() && !now.Before(entry.expiry) {
				s.removeElement(element)
				removed++
			}
		}
		element = next
	}
	return removed
}

// Len reports the number of live bindings, sweeping expired ones first.
func (s *Sessions) Len() int {
	if s == nil {
		return 0
	}
	s.Sweep()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.order.Len()
}

// Reset drops every binding.
func (s *Sessions) Reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = make(map[string]*list.Element)
	s.order = list.New()
}
