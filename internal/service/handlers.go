package service

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"opencodego-pool/internal/hostbridge"
	"opencodego-pool/internal/pool"
	"opencodego-pool/internal/usage"
)

const (
	pluginVersion = "0.1.0"
	// sessionHeaderName is the client-provided stable session identifier. The host
	// does not recognize it as a session signal, so stickiness is implemented here.
	sessionHeaderName = "X-Opencode-Session"
)

// registrationResponse mirrors the host's rpcRegistration wire shape.
type registrationResponse struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

// capabilities mirrors the host's rpcCapabilities wire shape. Only the fields
// this plugin sets are declared; the rest default to false.
type capabilities struct {
	Scheduler     bool `json:"scheduler"`
	UsagePlugin   bool `json:"usage_plugin"`
	ManagementAPI bool `json:"management_api"`
	QuotaProvider bool `json:"quota_provider"`
}

// identifierResponse mirrors the host's rpcIdentifierResponse wire shape.
type identifierResponse struct {
	Identifier string `json:"identifier"`
}

func (s *Service) metadata() pluginapi.Metadata {
	cfg := s.currentConfig()
	return pluginapi.Metadata{
		Name:             s.pluginID,
		Version:          pluginVersion,
		Author:           cfg.Author,
		GitHubRepository: cfg.Repository,
		Logo:             "",
		ConfigFields: []pluginapi.ConfigField{
			{Name: "usage_url", Type: pluginapi.ConfigFieldTypeString, Description: "opencode usage endpoint, e.g. https://opencode.ai/zen/go/v1/usage. Required."},
			{Name: "config_path", Type: pluginapi.ConfigFieldTypeString, Description: "Path to CLIProxyAPI's config.yaml. Required: the only source of plaintext api-keys."},
			{Name: "base_url_prefix", Type: pluginapi.ConfigFieldTypeString, Description: "Providers whose base-url starts with this (case-insensitive) belong to the pool."},
			{Name: "poll_interval", Type: pluginapi.ConfigFieldTypeString, Description: "Background usage refresh interval, e.g. 600s."},
			{Name: "session_ttl", Type: pluginapi.ConfigFieldTypeString, Description: "Sliding lifetime of a session→credential binding, e.g. 24h."},
			{Name: "max_sessions", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum number of tracked session bindings; least recently used are evicted."},
			{Name: "request_timeout", Type: pluginapi.ConfigFieldTypeString, Description: "Per-request deadline for the usage endpoint."},
			{Name: "stale_after", Type: pluginapi.ConfigFieldTypeString, Description: "Usage data older than this sorts last."},
			{Name: "log_level", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"debug", "info", "warn", "error"}, Description: "Plugin log verbosity."},
			{Name: "exclude_keys", Type: pluginapi.ConfigFieldTypeString, Description: "Credentials to keep out of routing. Accepts the api-key, auth id, or token."},
			{Name: "pin_sessions", Type: pluginapi.ConfigFieldTypeString, Description: "Map of session id to credential reference, forcing a binding."},
		},
	}
}

// Identify answers quota.identifier.
func (s *Service) Identify() identifierResponse {
	return identifierResponse{Identifier: s.pluginID}
}

// Pick answers scheduler.pick.
//
// It must never return an error: the host aborts credential selection when a
// scheduler rejects, which fails the client request outright. Every unexpected
// condition therefore degrades to Handled=false and falls back to the built-in
// scheduler.
func (s *Service) Pick(raw []byte) (pluginapi.SchedulerPickResponse, error) {
	var request pluginapi.SchedulerPickRequest
	if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
		s.log(hostbridge.LevelError, "scheduler.pick payload could not be decoded", map[string]any{
			"error": errUnmarshal.Error(),
		})
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}

	ready, reasons := s.routingReady()
	if !ready {
		s.stats.Unhandled.Add(1)
		s.logThrottled("routing-not-ready", 2*time.Minute, hostbridge.LevelError,
			"declining scheduler.pick; the host scheduler stays in charge", map[string]any{
				"reasons": strings.Join(reasons, "; "),
			})
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}

	candidates := make([]pool.Candidate, 0, len(request.Candidates))
	for _, candidate := range request.Candidates {
		candidates = append(candidates, pool.Candidate{ID: candidate.ID, Attributes: candidate.Attributes})
	}

	result := s.selector.Pick(pool.Input{
		Session:    headerValue(request.Options.Headers, sessionHeaderName),
		Candidates: candidates,
	}, s.currentSettings())

	switch result.Outcome {
	case pool.OutcomeMappingFailed:
		s.stats.MappingFailed.Add(1)
		s.logThrottled("mapping-failed", 2*time.Minute, hostbridge.LevelError,
			"candidates targeted the pool but none matched a known credential; declining this pick",
			map[string]any{"reason": result.Reason})
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	case pool.OutcomeUnhandled:
		s.stats.Unhandled.Add(1)
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}

	switch result.Outcome {
	case pool.OutcomeBound:
		s.stats.Bound.Add(1)
	case pool.OutcomeAssigned:
		s.stats.Assigned.Add(1)
	case pool.OutcomeRebound:
		s.stats.Rebound.Add(1)
		s.logIfEnabled(hostbridge.LevelInfo, "rebound session to a different credential", map[string]any{
			"token":  result.Token,
			"reason": result.Reason,
		})
	case pool.OutcomePinned:
		s.stats.Pinned.Add(1)
	case pool.OutcomeFallback:
		s.stats.Fallback.Add(1)
		s.logThrottled("fallback", time.Minute, hostbridge.LevelWarn,
			"every credential is flagged unusable; released the least exhausted one", nil)
	}

	return pluginapi.SchedulerPickResponse{AuthID: result.AuthID, Handled: true}, nil
}

// HandleUsage answers usage.handle.
//
// It only acts on credentials that failed upstream with 429: that is the fastest
// signal that a key just ran out, and it shortens the poll interval's blind spot
// from ten minutes to roughly the length of one request.
func (s *Service) HandleUsage(raw []byte) error {
	var record pluginapi.UsageRecord
	if errUnmarshal := json.Unmarshal(raw, &record); errUnmarshal != nil {
		return fmt.Errorf("decode usage record: %w", errUnmarshal)
	}
	if !record.Failed || record.Failure.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	token := s.tokenForAuthID(record.AuthID)
	if token == "" {
		return nil
	}
	s.note429(token)
	return nil
}

// DescribeQuota answers quota.describe.
func (s *Service) DescribeQuota() pluginapi.QuotaDescribeResponse {
	seen := make(map[string]struct{})
	providers := make([]string, 0, 2)
	for _, state := range s.registry.Snapshot() {
		if _, ok := seen[state.Provider]; ok || state.Provider == "" {
			continue
		}
		seen[state.Provider] = struct{}{}
		providers = append(providers, state.Provider)
	}
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: providers,
		DisplayName:        "OpenCode Pool",
		SupportsReset:      false,
	}
}

// FetchQuota answers quota.fetch from cached usage only; it never calls upstream.
func (s *Service) FetchQuota(raw []byte) (pluginapi.QuotaFetchResponse, error) {
	var request pluginapi.QuotaFetchRequest
	if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("decode quota request: %w", errUnmarshal)
	}

	apiKey := ""
	if request.Attributes != nil {
		apiKey = request.Attributes["api_key"]
	}
	state, ok := s.lookupCredential(request.AuthID, apiKey)
	if !ok {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf(
			"credential is not managed by %s", s.pluginID)
	}
	if !state.HasUsage {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf(
			"no usage cached yet for this credential; the next poll will populate it")
	}
	return quotaResponse(state), nil
}

// ResetQuota answers quota.reset.
func (s *Service) ResetQuota() pluginapi.QuotaResetResponse {
	return pluginapi.QuotaResetResponse{
		Success: false,
		Message: "upstream quota cannot be reset by this plugin",
	}
}

func quotaResponse(state pool.State) pluginapi.QuotaFetchResponse {
	buckets := make([]pluginapi.QuotaBucket, 0, 3)
	for _, item := range []struct {
		name   string
		window usage.Window
	}{
		{"rolling", state.Usage.Rolling},
		{"weekly", state.Usage.Weekly},
		{"monthly", state.Usage.Monthly},
	} {
		if !item.window.HasPercent {
			continue
		}
		resetTime := ""
		if item.window.HasResetsAt {
			resetTime = item.window.ResetsAt.Format(time.RFC3339)
		}
		buckets = append(buckets, pluginapi.QuotaBucket{
			Window:            item.name,
			RemainingFraction: clampFraction((100 - item.window.Percent) / 100),
			ResetTime:         resetTime,
			Description:       windowDescription(item.window),
		})
	}

	summary := make([]pluginapi.QuotaMetric, 0, 3)
	for _, item := range []struct {
		key    string
		label  string
		window usage.Window
	}{
		{"rolling_percent", "Rolling used", state.Usage.Rolling},
		{"weekly_percent", "Weekly used", state.Usage.Weekly},
		{"monthly_percent", "Monthly used", state.Usage.Monthly},
	} {
		if !item.window.HasPercent {
			continue
		}
		summary = append(summary, pluginapi.QuotaMetric{
			Key:    item.key,
			Label:  item.label,
			Value:  item.window.Percent,
			Unit:   "%",
			Format: "number",
		})
	}

	response := pluginapi.QuotaFetchResponse{}
	if len(summary) > 0 {
		response.Summary = summary
	}
	if len(buckets) > 0 {
		response.Groups = []pluginapi.QuotaGroup{{
			DisplayName: "opencode",
			Buckets:     buckets,
		}}
	}
	if !state.FetchedAt.IsZero() {
		response.ServerTimeOffsetMs = time.Since(state.FetchedAt).Milliseconds() * -1
	}
	return response
}

func windowDescription(window usage.Window) string {
	status := strings.TrimSpace(window.Status)
	if status == "" || strings.EqualFold(status, "ok") {
		return "ok"
	}
	return status
}

func clampFraction(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	if math.IsNaN(value) {
		return 0
	}
	return value
}

// tokenForAuthID recovers the credential token from a host auth record ID, which
// is shaped "<kind>:<token>".
func (s *Service) tokenForAuthID(authID string) string {
	trimmed := strings.TrimSpace(authID)
	if trimmed == "" {
		return ""
	}
	index := strings.LastIndexByte(trimmed, ':')
	if index < 0 || index == len(trimmed)-1 {
		return ""
	}
	token := strings.TrimSpace(trimmed[index+1:])
	if token == "" || !s.registry.IsTracked(token) {
		return ""
	}
	return token
}

// lookupCredential resolves a quota request back to a tracked credential.
func (s *Service) lookupCredential(authID, apiKey string) (pool.State, bool) {
	if token := s.tokenForAuthID(authID); token != "" {
		if state, ok := s.registry.Get(token); ok {
			return state, true
		}
	}
	normalizedKey := strings.TrimSpace(apiKey)
	if normalizedKey == "" {
		return pool.State{}, false
	}
	for _, state := range s.registry.Snapshot() {
		if state.APIKey == normalizedKey {
			return state, true
		}
	}
	return pool.State{}, false
}

// headerValue performs a case-insensitive lookup of a single header value.
//
// The host marshals an http.Header, so keys usually arrive canonicalized, but a
// case-insensitive scan removes any dependency on that.
func headerValue(headers map[string][]string, name string) string {
	for key, values := range headers {
		if !strings.EqualFold(strings.TrimSpace(key), name) {
			continue
		}
		for _, value := range values {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// logThrottled emits at most one line per key per window, so a hot path cannot
// flood the host log.
func (s *Service) logThrottled(key string, window time.Duration, level, message string, fields map[string]any) {
	if !s.logEnabled(level) {
		return
	}
	now := s.now()
	s.logMu.Lock()
	last, seen := s.logThrottle[key]
	allowed := !seen || now.Sub(last) >= window
	if allowed {
		s.logThrottle[key] = now
	}
	s.logMu.Unlock()
	if allowed {
		s.log(level, message, fields)
	}
}
