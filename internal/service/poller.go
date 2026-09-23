package service

import (
	"time"

	"opencodego-pool/internal/hostbridge"
	"opencodego-pool/internal/pluginconfig"
)

const (
	// queueDepth bounds the refresh backlog. A full queue drops the request; the
	// next poll cycle re-enqueues it, so nothing is lost permanently.
	queueDepth = 1024
	// shutdownJoinTimeout bounds how long Shutdown waits for host calls that were
	// abandoned by a soft timeout. The host exposes no timeout for host.http.do,
	// so a stalled request can outlive our deadline.
	shutdownJoinTimeout = 30 * time.Second
	// pruneFailStreak is how many consecutive failed usage fetches a credential
	// must accumulate, while never appearing as a candidate, before it is dropped.
	pruneFailStreak = 3
)

// Start launches the poller. It is idempotent.
func (s *Service) Start() {
	s.startOnce.Do(func() {
		for i := 0; i < pluginconfig.DefaultConcurrency; i++ {
			s.workers.Add(1)
			go s.worker()
		}
		s.workers.Add(1)
		go s.pollLoop()
	})
}

// Stop terminates the poller and waits for the worker goroutines. It is idempotent.
//
// Workers that were inside a usage fetch when the stop signal arrived have
// already handed it off to a detached goroutine tracked by fetchWG; Shutdown
// waits for those separately.
func (s *Service) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	s.workers.Wait()
}

func (s *Service) worker() {
	defer s.workers.Done()
	for {
		select {
		case <-s.stopCh:
			return
		default:
		}
		select {
		case <-s.stopCh:
			return
		case token := <-s.queue:
			s.refreshOne(token)
		}
	}
}

// pollLoop drives the periodic usage refresh, the config.yaml mtime watch, and
// the session sweep.
//
// The config watch is a safety net: the host triggers plugin.reconfigure on every
// config.yaml reload, but a reload that fails midway never reaches us, so the
// file is also polled directly.
func (s *Service) pollLoop() {
	defer s.workers.Done()

	interval := s.pollInterval()
	pollTicker := time.NewTicker(interval)
	defer pollTicker.Stop()

	configTicker := time.NewTicker(pluginconfig.DefaultConfigPollEvery)
	defer configTicker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-configTicker.C:
			s.syncConfigFile(false)
		case <-pollTicker.C:
			s.sweep()
			s.refreshAll()
			if next := s.pollInterval(); next != interval {
				interval = next
				pollTicker.Reset(next)
			}
		}
	}
}

func (s *Service) pollInterval() time.Duration {
	interval := s.currentConfig().PollInterval.Std()
	if interval <= 0 {
		interval = pluginconfig.DefaultPollInterval
	}
	return interval
}

// refreshAll enqueues every tracked credential for a usage refresh.
func (s *Service) refreshAll() {
	for _, state := range s.registry.Snapshot() {
		s.requestRefresh(state.Token, false)
	}
}

// requestRefresh enqueues a usage refresh for one credential.
//
// force bypasses the per-credential throttle; it is used for newly discovered
// keys and for the 429 fast path, which already applies its own throttle.
func (s *Service) requestRefresh(token string, force bool) {
	if token == "" {
		return
	}
	now := s.now()

	s.queueMu.Lock()
	defer s.queueMu.Unlock()

	if _, busy := s.inflight[token]; busy {
		return
	}
	if !force {
		if last, seen := s.lastAttempt[token]; seen && now.Sub(last) < pluginconfig.DefaultSuspectThrottle {
			return
		}
	}
	select {
	case s.queue <- token:
		s.inflight[token] = struct{}{}
		s.lastAttempt[token] = now
	default:
		// Backlog full: drop it, the next poll cycle will retry.
	}
}

// note429 reacts to a live request failing with 429 by marking the credential
// suspect and asking for an out-of-band refresh, throttled per credential so a
// burst of failures cannot turn into a burst of upstream requests.
func (s *Service) note429(token string) {
	if token == "" {
		return
	}
	now := s.now()
	s.registry.MarkSuspected429(token, now)
	s.stats.Suspected429.Add(1)

	s.suspectMu.Lock()
	last, seen := s.lastSuspect[token]
	allowed := !seen || now.Sub(last) >= pluginconfig.DefaultSuspectThrottle
	if allowed {
		s.lastSuspect[token] = now
	}
	s.suspectMu.Unlock()

	if allowed {
		s.requestRefresh(token, true)
	}
}

// sweep expires session bindings and drops credentials that are both invisible
// to the scheduler and unreachable on the usage endpoint.
func (s *Service) sweep() {
	s.sessions.Sweep()

	window := 3 * s.pollInterval()
	if window <= 0 {
		return
	}
	cutoff := s.now().Add(-window)
	for _, state := range s.registry.Snapshot() {
		if state.LastSeenAt.IsZero() || state.LastSeenAt.After(cutoff) {
			continue
		}
		if state.FailStreak < pruneFailStreak {
			continue
		}
		s.registry.Remove(state.Token)
		s.forgetCredential(state.Token)
		s.logIfEnabled(hostbridge.LevelWarn, "credential dropped: never offered as a candidate and its usage endpoint kept failing", map[string]any{
			"token": state.Token,
		})
	}
}
