// Package mgmtui renders the read-only view of the credential pool.
//
// The host serves no static files for plugins, so the page is embedded in the
// shared object and returned by the plugin itself. It performs no privileged
// action: every value it shows is already redacted server-side.
package mgmtui

// Snapshot is the shared shape of the JSON route and the resource page.
type Snapshot struct {
	PluginID      string   `json:"plugin_id"`
	Version       string   `json:"version"`
	Healthy       bool     `json:"healthy"`
	Problems      []string `json:"problems,omitempty"`
	UsageURL      string   `json:"usage_url,omitempty"`
	ConfigPath    string   `json:"config_path,omitempty"`
	BaseURLPrefix string   `json:"base_url_prefix"`
	PollInterval  string   `json:"poll_interval"`
	StaleAfter    string   `json:"stale_after"`
	SessionTTL    string   `json:"session_ttl"`
	MaxSessions   int      `json:"max_sessions"`
	Sessions      int      `json:"sessions"`
	Keys          []Key    `json:"keys"`
	Stats         Stats    `json:"stats"`
	GeneratedAt   string   `json:"generated_at"`
}

// Key is one credential's public state. The api-key itself never appears here.
type Key struct {
	Token        string  `json:"token"`
	AuthID       string  `json:"auth_id"`
	Name         string  `json:"name"`
	Provider     string  `json:"provider"`
	BaseURL      string  `json:"base_url"`
	Usable       bool    `json:"usable"`
	Exclusion    string  `json:"exclusion,omitempty"`
	HasUsage     bool    `json:"has_usage"`
	Rolling      *Window `json:"rolling,omitempty"`
	Weekly       *Window `json:"weekly,omitempty"`
	Monthly      *Window `json:"monthly,omitempty"`
	FetchedAt    string  `json:"fetched_at,omitempty"`
	FailStreak   int     `json:"fail_streak"`
	Revoked      bool    `json:"revoked"`
	Suspected429 bool    `json:"suspected_429"`
	Sessions     int     `json:"sessions"`
	LastSeenAt   string  `json:"last_seen_at,omitempty"`
}

// Window is one quota window as the plugin saw it.
type Window struct {
	Status     string  `json:"status"`
	Percent    float64 `json:"percent"`
	HasPercent bool    `json:"has_percent"`
	ResetsAt   string  `json:"resets_at,omitempty"`
}

// Stats are the cumulative routing counters.
type Stats struct {
	Picks         int64 `json:"picks"`
	Bound         int64 `json:"bound"`
	Assigned      int64 `json:"assigned"`
	Rebound       int64 `json:"rebound"`
	Pinned        int64 `json:"pinned"`
	Fallback      int64 `json:"fallback"`
	Unhandled     int64 `json:"unhandled"`
	MappingFailed int64 `json:"mapping_failed"`

	Fetches       int64 `json:"fetches"`
	FetchFailures int64 `json:"fetch_failures"`
	FetchTimeouts int64 `json:"fetch_timeouts"`
	Revoked       int64 `json:"revoked"`
	Suspected429  int64 `json:"suspected_429"`

	ConfigSyncs      int64 `json:"config_syncs"`
	ConfigSyncErrors int64 `json:"config_sync_errors"`
}
