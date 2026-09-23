package service

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"opencodego-pool/internal/mgmtui"
	"opencodego-pool/internal/usage"
)

const (
	stateRouteSuffix     = "/state"
	statusResourceSuffix = "/status"
)

// managementRegistrationResponse mirrors the host's rpcManagementRegistrationResponse.
type managementRegistrationResponse struct {
	Routes    []pluginapi.ManagementRoute `json:"routes,omitempty"`
	Resources []pluginapi.ResourceRoute   `json:"resources,omitempty"`
}

// RegisterManagement answers management.register.
//
// The JSON route lands under /v0/management and requires the management key; the
// resource is served under /v0/resource/plugins/<id>/ and is read-only.
func (s *Service) RegisterManagement() managementRegistrationResponse {
	return managementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{{
			Method:      http.MethodGet,
			Path:        "/plugins/" + s.pluginID + stateRouteSuffix,
			Description: "JSON snapshot of the opencode credential pool.",
		}},
		Resources: []pluginapi.ResourceRoute{{
			Path:        statusResourceSuffix,
			Menu:        "OpenCode Pool",
			Description: "Read-only view of per-key usage, exclusion reasons, and session bindings.",
		}},
	}
}

// HandleManagement answers management.handle.
func (s *Service) HandleManagement(raw []byte) (pluginapi.ManagementResponse, error) {
	var request pluginapi.ManagementRequest
	if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
		return pluginapi.ManagementResponse{}, errUnmarshal
	}

	path := strings.TrimSuffix(strings.TrimSpace(request.Path), "/")
	snapshot := s.StateSnapshot()

	switch {
	case strings.HasSuffix(path, stateRouteSuffix):
		body, errMarshal := json.MarshalIndent(snapshot, "", "  ")
		if errMarshal != nil {
			return pluginapi.ManagementResponse{}, errMarshal
		}
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
			Body:       body,
		}, nil
	case strings.HasSuffix(path, statusResourceSuffix):
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
			Body:       mgmtui.Page(snapshot),
		}, nil
	}

	return pluginapi.ManagementResponse{
		StatusCode: http.StatusNotFound,
		Headers:    http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		Body:       []byte(s.pluginID + ": no handler for " + path),
	}, nil
}

// StateSnapshot builds the read-only view shared by the JSON route and the page.
//
// It never exposes api-key material: the token is the only credential identifier
// that leaves this function.
func (s *Service) StateSnapshot() mgmtui.Snapshot {
	cfg := s.currentConfig()
	settings := s.currentSettings()
	now := s.now()

	healthy, problems := s.routingReady()
	snapshot := mgmtui.Snapshot{
		PluginID:      s.pluginID,
		Version:       pluginVersion,
		Healthy:       healthy,
		Problems:      problems,
		UsageURL:      cfg.UsageURL,
		ConfigPath:    cfg.ConfigPath,
		BaseURLPrefix: cfg.BaseURLPrefix,
		PollInterval:  cfg.PollInterval.Std().String(),
		StaleAfter:    cfg.StaleAfter.Std().String(),
		SessionTTL:    cfg.SessionTTL.Std().String(),
		MaxSessions:   cfg.MaxSessions,
		Sessions:      s.sessions.Len(),
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Stats: mgmtui.Stats{
			Picks:         s.stats.Picks.Load(),
			Bound:         s.stats.Bound.Load(),
			Assigned:      s.stats.Assigned.Load(),
			Rebound:       s.stats.Rebound.Load(),
			Pinned:        s.stats.Pinned.Load(),
			Fallback:      s.stats.Fallback.Load(),
			Unhandled:     s.stats.Unhandled.Load(),
			MappingFailed: s.stats.MappingFailed.Load(),

			Fetches:       s.stats.Fetches.Load(),
			FetchFailures: s.stats.FetchFailures.Load(),
			FetchTimeouts: s.stats.FetchTimeouts.Load(),
			Revoked:       s.stats.Revoked.Load(),
			Suspected429:  s.stats.Suspected429.Load(),

			ConfigSyncs:      s.stats.ConfigSyncs.Load(),
			ConfigSyncErrors: s.stats.ConfigSyncErrors.Load(),
		},
	}

	for _, state := range s.registry.Snapshot() {
		exclusion := s.selector.ExclusionReason(state, settings, now)
		key := mgmtui.Key{
			Token:        state.Token,
			AuthID:       state.AuthID,
			Name:         state.Name,
			Provider:     state.Provider,
			BaseURL:      state.BaseURL,
			Usable:       exclusion == "",
			Exclusion:    exclusion,
			HasUsage:     state.HasUsage,
			FailStreak:   state.FailStreak,
			Revoked:      state.Revoked,
			Suspected429: state.Suspected429(now),
			Sessions:     s.sessions.CountToken(state.Token),
			Rolling:      windowView(state.Usage.Rolling, state.HasUsage),
			Weekly:       windowView(state.Usage.Weekly, state.HasUsage),
			Monthly:      windowView(state.Usage.Monthly, state.HasUsage),
		}
		if !state.FetchedAt.IsZero() {
			key.FetchedAt = state.FetchedAt.UTC().Format(time.RFC3339)
		}
		if !state.LastSeenAt.IsZero() {
			key.LastSeenAt = state.LastSeenAt.UTC().Format(time.RFC3339)
		}
		snapshot.Keys = append(snapshot.Keys, key)
	}
	return snapshot
}

func windowView(window usage.Window, hasUsage bool) *mgmtui.Window {
	if !hasUsage {
		return nil
	}
	view := &mgmtui.Window{
		Status:     window.Status,
		Percent:    window.Percent,
		HasPercent: window.HasPercent,
	}
	if window.HasResetsAt {
		view.ResetsAt = window.ResetsAt.UTC().Format(time.RFC3339)
	}
	return view
}
