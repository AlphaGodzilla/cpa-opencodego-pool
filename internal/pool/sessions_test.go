package pool

import (
	"testing"
	"time"
)

type testClock struct{ current time.Time }

func newTestClock() *testClock {
	return &testClock{current: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) now() time.Time          { return c.current }
func (c *testClock) advance(d time.Duration) { c.current = c.current.Add(d) }

func TestSessionsBindAndGet(t *testing.T) {
	clock := newTestClock()
	sessions := NewSessions(time.Hour, 10, clock.now)

	if _, ok := sessions.Get("s1", "p1"); ok {
		t.Fatal("an unknown session must not resolve")
	}
	sessions.Bind("s1", "p1", "tok-a")
	token, ok := sessions.Get("s1", "p1")
	if !ok || token != "tok-a" {
		t.Fatalf("Get = %q/%v, want tok-a/true", token, ok)
	}
}

func TestSessionsAreScopedPerProvider(t *testing.T) {
	clock := newTestClock()
	sessions := NewSessions(time.Hour, 10, clock.now)

	sessions.Bind("s1", "provider-a", "tok-a")
	sessions.Bind("s1", "provider-b", "tok-b")

	if token, _ := sessions.Get("s1", "provider-a"); token != "tok-a" {
		t.Errorf("provider-a = %q", token)
	}
	if token, _ := sessions.Get("s1", "provider-b"); token != "tok-b" {
		t.Errorf("provider-b = %q", token)
	}
}

func TestSessionsExpireAndSweep(t *testing.T) {
	clock := newTestClock()
	sessions := NewSessions(time.Hour, 10, clock.now)
	sessions.Bind("s1", "p1", "tok-a")

	clock.advance(30 * time.Minute)
	if _, ok := sessions.Get("s1", "p1"); !ok {
		t.Fatal("binding should still be live before its TTL")
	}

	clock.advance(2 * time.Hour)
	if _, ok := sessions.Get("s1", "p1"); ok {
		t.Fatal("binding should have expired")
	}

	sessions.Bind("s2", "p1", "tok-b")
	clock.advance(2 * time.Hour)
	if removed := sessions.Sweep(); removed != 1 {
		t.Fatalf("Sweep removed %d, want 1", removed)
	}
	if sessions.Len() != 0 {
		t.Fatalf("Len = %d, want 0", sessions.Len())
	}
}

func TestSessionsGetRenewsTTL(t *testing.T) {
	clock := newTestClock()
	sessions := NewSessions(time.Hour, 10, clock.now)
	sessions.Bind("s1", "p1", "tok-a")

	for i := 0; i < 5; i++ {
		clock.advance(50 * time.Minute)
		if _, ok := sessions.Get("s1", "p1"); !ok {
			t.Fatalf("binding expired on renewal %d", i)
		}
	}
}

func TestSessionsLRUEviction(t *testing.T) {
	clock := newTestClock()
	sessions := NewSessions(time.Hour, 3, clock.now)

	sessions.Bind("s1", "p1", "tok-1")
	sessions.Bind("s2", "p1", "tok-2")
	sessions.Bind("s3", "p1", "tok-3")

	// Touching s1 makes s2 the least recently used.
	if _, ok := sessions.Get("s1", "p1"); !ok {
		t.Fatal("s1 should be live")
	}
	sessions.Bind("s4", "p1", "tok-4")

	if _, ok := sessions.Get("s2", "p1"); ok {
		t.Error("s2 should have been evicted")
	}
	for _, session := range []string{"s1", "s3", "s4"} {
		if _, ok := sessions.Get(session, "p1"); !ok {
			t.Errorf("%s should have survived", session)
		}
	}
	if sessions.Len() != 3 {
		t.Fatalf("Len = %d, want 3", sessions.Len())
	}
}

func TestSessionsConfigureResizesInPlace(t *testing.T) {
	clock := newTestClock()
	sessions := NewSessions(time.Hour, 10, clock.now)
	for _, session := range []string{"s1", "s2", "s3"} {
		sessions.Bind(session, "p1", "tok")
	}

	sessions.Configure(2*time.Hour, 2)
	if sessions.Len() != 2 {
		t.Fatalf("Len = %d, want 2 after shrinking the bound", sessions.Len())
	}

	// The new TTL applies to bindings created from now on.
	sessions.Bind("s4", "p1", "tok-new")
	clock.advance(90 * time.Minute)
	if _, ok := sessions.Get("s4", "p1"); !ok {
		t.Error("the configured TTL should carry s4 past the original one")
	}
}

func TestSessionsDropTokenAndCount(t *testing.T) {
	clock := newTestClock()
	sessions := NewSessions(time.Hour, 10, clock.now)

	sessions.Bind("s1", "p1", "tok-a")
	sessions.Bind("s2", "p1", "tok-a")
	sessions.Bind("s3", "p1", "tok-b")

	if count := sessions.CountToken("tok-a"); count != 2 {
		t.Fatalf("CountToken(a) = %d, want 2", count)
	}
	if dropped := sessions.DropToken("tok-a"); dropped != 2 {
		t.Fatalf("DropToken(a) = %d, want 2", dropped)
	}
	if sessions.CountToken("tok-a") != 0 {
		t.Error("bindings should be gone")
	}
	if sessions.CountToken("tok-b") != 1 {
		t.Error("unrelated bindings must survive")
	}
}

func TestSessionsRebindReplacesToken(t *testing.T) {
	clock := newTestClock()
	sessions := NewSessions(time.Hour, 10, clock.now)

	sessions.Bind("s1", "p1", "tok-a")
	sessions.Bind("s1", "p1", "tok-b")

	if token, _ := sessions.Get("s1", "p1"); token != "tok-b" {
		t.Fatalf("token = %q, want tok-b", token)
	}
	if sessions.CountToken("tok-a") != 0 {
		t.Error("the previous token must not keep the binding")
	}
	if sessions.Len() != 1 {
		t.Fatalf("Len = %d, want 1", sessions.Len())
	}
}

func TestSessionsIgnoreEmptyToken(t *testing.T) {
	sessions := NewSessions(time.Hour, 10, nil)
	sessions.Bind("s1", "p1", "")
	if sessions.Len() != 0 {
		t.Fatal("binding an empty token must be a no-op")
	}
}

func TestSessionsResetClearsEverything(t *testing.T) {
	sessions := NewSessions(time.Hour, 10, nil)
	sessions.Bind("s1", "p1", "tok-a")
	sessions.Reset()
	if sessions.Len() != 0 {
		t.Fatal("Reset should drop every binding")
	}
}
