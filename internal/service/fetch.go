package service

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"opencodego-pool/internal/hostbridge"
	"opencodego-pool/internal/usage"
)

type fetchResult struct {
	status int
	body   []byte
	err    error
}

// refreshOne performs one usage fetch and folds the outcome into the registry.
func (s *Service) refreshOne(token string) {
	defer func() {
		s.queueMu.Lock()
		delete(s.inflight, token)
		s.queueMu.Unlock()
	}()

	state, ok := s.registry.Get(token)
	if !ok {
		return
	}
	s.fetchUsage(token, state.APIKey)
}

// fetchUsage queries the opencode usage endpoint for one credential.
//
// host.http.do exposes no timeout, so a deadline is imposed here. When it fires
// the worker moves on but the underlying call keeps running: it is tracked in
// fetchWG so Shutdown can wait for it instead of letting it touch a host context
// that is about to be freed.
func (s *Service) fetchUsage(token, apiKey string) {
	cfg := s.currentConfig()
	if cfg.UsageURL == "" || apiKey == "" {
		return
	}
	s.stats.Fetches.Add(1)

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+apiKey)
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", "opencode-go-quota")

	timeout := cfg.RequestTimeout.Std()
	if timeout <= 0 {
		timeout = DefaultTimeoutFallback
	}

	s.fetchWG.Add(1)
	done := make(chan fetchResult, 1)
	go func() {
		defer s.fetchWG.Done()
		status, _, body, errCall := hostbridge.HTTPDo(context.Background(), http.MethodGet, cfg.UsageURL, headers, nil)
		done <- fetchResult{status: status, body: body, err: errCall}
	}()

	select {
	case result := <-done:
		s.applyFetchResult(token, result)
	case <-time.After(timeout):
		s.stats.FetchTimeouts.Add(1)
		s.stats.FetchFailures.Add(1)
		s.registry.StoreFetchFailure(token)
		s.logIfEnabled(hostbridge.LevelWarn, "usage fetch timed out", map[string]any{
			"token":   token,
			"timeout": timeout.String(),
		})
	}
}

const DefaultTimeoutFallback = 15 * time.Second

func (s *Service) applyFetchResult(token string, result fetchResult) {
	if result.err != nil {
		s.stats.FetchFailures.Add(1)
		s.registry.StoreFetchFailure(token)
		s.logIfEnabled(hostbridge.LevelDebug, "usage fetch failed", map[string]any{
			"token": token,
			"error": result.err.Error(),
		})
		return
	}

	switch result.status {
	case http.StatusUnauthorized, http.StatusForbidden:
		s.stats.Revoked.Add(1)
		s.registry.StoreRevoked(token, fmt.Sprintf("usage endpoint returned %d", result.status))
		s.log(hostbridge.LevelError, "credential rejected by the usage endpoint; excluding it from routing", map[string]any{
			"token":  token,
			"status": result.status,
		})
		return
	}

	if result.status < 200 || result.status >= 300 {
		s.stats.FetchFailures.Add(1)
		s.registry.StoreFetchFailure(token)
		s.logIfEnabled(hostbridge.LevelDebug, "usage fetch returned an unexpected status", map[string]any{
			"token":  token,
			"status": result.status,
		})
		return
	}

	parsed, errParse := usage.Parse(result.body)
	if errParse != nil {
		s.stats.FetchFailures.Add(1)
		s.registry.StoreFetchFailure(token)
		s.log(hostbridge.LevelError, "usage payload could not be parsed; the upstream shape may have changed", map[string]any{
			"token": token,
			"error": errParse.Error(),
		})
		return
	}

	s.registry.StoreUsage(token, parsed, s.now())
	if unavailable, reason := parsed.Unavailable(); unavailable {
		s.logIfEnabled(hostbridge.LevelDebug, "credential flagged unusable by upstream", map[string]any{
			"token":  token,
			"reason": reason,
		})
	}
}
