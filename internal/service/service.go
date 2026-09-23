// Package service wires the pure routing logic in internal/pool to the host:
// it owns the credential table's config source, the background usage poller, and
// every RPC method the host can call.
package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"opencodego-pool/internal/hostbridge"
	"opencodego-pool/internal/keysource"
	"opencodego-pool/internal/pluginconfig"
	"opencodego-pool/internal/pool"
)

// SchemaVersion is the RPC JSON contract this plugin speaks. The host rejects a
// plugin that declares a higher version than it knows.
const SchemaVersion uint32 = 6

// DefaultPluginID matches the shared-object file name the host derives the
// plugin ID from.
const DefaultPluginID = "opencodego-pool"

type fileStamp struct {
	modTime time.Time
	size    int64
	valid   bool
}

// Service is the plugin's runtime state.
type Service struct {
	pluginID string
	now      func() time.Time

	cfgMu    sync.RWMutex
	cfg      pluginconfig.Config
	problems []string

	registry *pool.Registry
	sessions *pool.Sessions
	selector *pool.Selector
	settings atomic.Pointer[pool.Settings]

	// configHealthy is false until config.yaml has been read and parsed at least
	// once, and false again whenever the most recent attempt failed.
	configHealthy atomic.Bool

	fileMu   sync.Mutex
	lastStat fileStamp

	stats Stats

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	queue     chan string
	workers   sync.WaitGroup
	fetchWG   sync.WaitGroup

	queueMu     sync.Mutex
	inflight    map[string]struct{}
	lastAttempt map[string]time.Time

	suspectMu   sync.Mutex
	lastSuspect map[string]time.Time

	logMu       sync.Mutex
	logThrottle map[string]time.Time

	registered atomic.Bool

	// hostSchema is the RPC contract version the host announced in its most recent
	// plugin.register / plugin.reconfigure request. Answering with it instead of a
	// hardcoded constant is what lets one build load on both older and newer hosts.
	hostSchema atomic.Uint32
}

// New creates a service. pluginID is normally the shared-object file name.
func New(pluginID string) *Service {
	pluginID = strings.TrimSpace(pluginID)
	if pluginID == "" {
		pluginID = DefaultPluginID
	}
	now := time.Now
	svc := &Service{
		pluginID:    pluginID,
		now:         now,
		registry:    pool.NewRegistry(now),
		sessions:    pool.NewSessions(pluginconfig.DefaultSessionTTL, pluginconfig.DefaultMaxSessions, now),
		stopCh:      make(chan struct{}),
		queue:       make(chan string, queueDepth),
		inflight:    make(map[string]struct{}),
		lastAttempt: make(map[string]time.Time),
		lastSuspect: make(map[string]time.Time),
		logThrottle: make(map[string]time.Time),
	}
	svc.selector = &pool.Selector{Registry: svc.registry, Sessions: svc.sessions, Now: now}
	svc.settings.Store(&pool.Settings{BaseURLPrefix: pluginconfig.DefaultBaseURLPrefix})
	return svc
}

// PluginID reports the ID this plugin registered under.
func (s *Service) PluginID() string { return s.pluginID }

func (s *Service) log(level, message string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["plugin_id"] = s.pluginID
	hostbridge.Log(level, message, fields)
}

func (s *Service) logIfEnabled(level, message string, fields map[string]any) {
	if !s.logEnabled(level) {
		return
	}
	s.log(level, message, fields)
}

func (s *Service) logEnabled(level string) bool {
	configured := strings.ToLower(strings.TrimSpace(s.currentConfig().LogLevel))
	switch configured {
	case "debug":
		return true
	case "info":
		return level != hostbridge.LevelDebug
	case "warn", "warning", "":
		return level == hostbridge.LevelWarn || level == hostbridge.LevelError
	case "error":
		return level == hostbridge.LevelError
	default:
		return level == hostbridge.LevelWarn || level == hostbridge.LevelError
	}
}

func (s *Service) currentConfig() pluginconfig.Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

func (s *Service) currentProblems() []string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return append([]string(nil), s.problems...)
}

func (s *Service) currentSettings() pool.Settings {
	if settings := s.settings.Load(); settings != nil {
		return *settings
	}
	return pool.Settings{BaseURLPrefix: pluginconfig.DefaultBaseURLPrefix}
}

// applyConfig stores a freshly parsed config and rebuilds the derived state.
//
// restartPoller is set when timing-sensitive knobs changed, which is not worth
// restarting goroutines for: the poll loop re-reads the interval every tick.
func (s *Service) applyConfig(cfg pluginconfig.Config) {
	problems := cfg.Problems()

	s.cfgMu.Lock()
	s.cfg = cfg
	s.problems = problems
	s.cfgMu.Unlock()

	s.sessions.Configure(cfg.SessionTTL.Std(), cfg.MaxSessions)

	if len(problems) > 0 {
		s.log(hostbridge.LevelError, "opencodego-pool is refusing to route: configuration is incomplete", map[string]any{
			"problems": problems,
		})
	}
}

// Register handles plugin.register: it parses the config, performs the first

// lifecycleRequest mirrors the host's rpcLifecycleRequest, which wraps the
// plugin's own config section in an envelope alongside the negotiated schema
// version. Parsing the payload directly as YAML would silently yield an empty
// config, so the unwrapping matters.
type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// parseLifecycleConfig unwraps the request the host sends for plugin.register
// and plugin.reconfigure.
//
// An absent body is valid: the host always sends the envelope, but a bare
// payload (hand-written test fixture) is decoded as raw YAML for convenience.
func (s *Service) parseLifecycleRequest(raw []byte) (pluginconfig.Config, uint32, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		cfg, errParse := pluginconfig.Parse(nil)
		return cfg, 0, errParse
	}
	if trimmed[0] != '{' {
		cfg, errParse := pluginconfig.Parse(raw)
		return cfg, 0, errParse
	}
	var request lifecycleRequest
	if errUnmarshal := json.Unmarshal(trimmed, &request); errUnmarshal != nil {
		return pluginconfig.Config{}, 0, fmt.Errorf("decode lifecycle request: %w", errUnmarshal)
	}
	cfg, errParse := pluginconfig.Parse(request.ConfigYAML)
	return cfg, request.SchemaVersion, errParse
}

// resolveSchemaVersion picks the RPC contract version to answer with.
//
// The host announces its own pluginabi.SchemaVersion in the register request and
// rejects any plugin that declares a higher one. Hardcoding the newest version
// this plugin knows would therefore make it refuse to load on an older host, and
// that refusal appears only in the host's log — the plugin itself sees a
// successful register call. Answering with the host's version, capped at what this
// plugin understands, keeps a single build working across host releases.
func resolveSchemaVersion(hostVersion uint32) uint32 {
	if hostVersion == 0 {
		// The host treats a missing version as the original contract.
		return 1
	}
	if hostVersion > SchemaVersion {
		return SchemaVersion
	}
	return hostVersion
}

// credential sync, and starts the background poller.
func (s *Service) Register(raw []byte) (registrationResponse, error) {
	cfg, hostSchema, errParse := s.parseLifecycleRequest(raw)
	if errParse != nil {
		return registrationResponse{}, errParse
	}
	s.hostSchema.Store(hostSchema)
	if hostSchema > 0 && hostSchema < SchemaVersion {
		// A capability the host silently ignores is exactly the kind of failure that
		// costs hours to diagnose, so say it out loud once, at load time.
		s.log(hostbridge.LevelWarn, "the host announces an older plugin schema version; capabilities added after it may be ignored", map[string]any{
			"host_schema_version":       hostSchema,
			"plugin_max_schema_version": SchemaVersion,
		})
	}
	s.applyConfig(cfg)
	s.syncConfigFile(true)
	s.Start()
	s.registered.Store(true)
	return s.registration(), nil
}

// Reconfigure handles plugin.reconfigure.
//
// The host calls this on every config.yaml reload and serializes it ahead of the
// credential diff, so it must return immediately: the file re-read is deferred.
func (s *Service) Reconfigure(raw []byte) (registrationResponse, error) {
	cfg, hostSchema, errParse := s.parseLifecycleRequest(raw)
	if errParse != nil {
		return registrationResponse{}, errParse
	}
	s.hostSchema.Store(hostSchema)
	s.applyConfig(cfg)
	go s.syncConfigFile(true)
	return s.registration(), nil
}

// Quiesce stops the background poller without tearing anything else down.
func (s *Service) Quiesce() {
	s.Stop()
}

// Shutdown stops the poller and waits for every goroutine this plugin started.
//
// The host only waits for its own in-flight host→plugin calls before invoking
// the C shutdown hook; goroutines the plugin spawned are not counted. Returning
// while one is still inside a host call would have it run against a freed host
// context, so this blocks (bounded) until they finish.
func (s *Service) Shutdown() {
	s.Stop()
	done := make(chan struct{})
	go func() {
		s.fetchWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownJoinTimeout):
		s.log(hostbridge.LevelError, "timed out waiting for in-flight host calls; shutting down anyway", nil)
	}
}

func (s *Service) registration() registrationResponse {
	return registrationResponse{
		SchemaVersion: resolveSchemaVersion(s.hostSchema.Load()),
		Metadata:      s.metadata(),
		Capabilities: capabilities{
			Scheduler:     true,
			UsagePlugin:   true,
			ManagementAPI: true,
			QuotaProvider: true,
		},
	}
}

// syncConfigFile re-reads CLIProxyAPI's config.yaml and reconciles the credential
// table. It is the only channel that yields plaintext api-keys, because the host
// redacts them from every scheduler candidate.
func (s *Service) syncConfigFile(force bool) {
	cfg := s.currentConfig()
	if cfg.ConfigPath == "" {
		s.configHealthy.Store(false)
		return
	}

	s.fileMu.Lock()
	defer s.fileMu.Unlock()

	info, errStat := os.Stat(cfg.ConfigPath)
	if errStat != nil {
		s.configHealthy.Store(false)
		s.stats.ConfigSyncErrors.Add(1)
		s.log(hostbridge.LevelError, "cannot stat config_path; refusing to route", map[string]any{
			"config_path": cfg.ConfigPath,
			"error":       errStat.Error(),
		})
		return
	}
	stamp := fileStamp{modTime: info.ModTime(), size: info.Size(), valid: true}
	if !force && s.lastStat.valid && s.lastStat.modTime.Equal(stamp.modTime) && s.lastStat.size == stamp.size {
		return
	}

	keys, errLoad := keysource.LoadKeys(cfg.ConfigPath, cfg.BaseURLPrefix)
	if errLoad != nil {
		s.configHealthy.Store(false)
		s.stats.ConfigSyncErrors.Add(1)
		s.log(hostbridge.LevelError, "cannot parse config_path; refusing to route", map[string]any{
			"config_path": cfg.ConfigPath,
			"error":       errLoad.Error(),
		})
		return
	}

	added, removed := s.registry.Sync(keys)
	s.lastStat = stamp
	s.configHealthy.Store(true)
	s.stats.ConfigSyncs.Add(1)

	for _, token := range removed {
		if released := s.forgetCredential(token); released > 0 {
			s.logIfEnabled(hostbridge.LevelInfo, "credential removed; released its session bindings", map[string]any{
				"token":    token,
				"released": released,
			})
		}
	}
	s.rebuildSettings()

	if len(added) > 0 {
		s.logIfEnabled(hostbridge.LevelInfo, "new credentials discovered", map[string]any{
			"tokens": strings.Join(added, ","),
		})
		for _, token := range added {
			s.requestRefresh(token, true)
		}
	}
}

// forgetCredential drops every trace of a credential that has left the
// configuration, and reports how many session bindings were released.
//
// A credential leaves by one of two paths — the config diff inside
// syncConfigFile and the sweep-based prune — so the cleanup lives here instead of
// being repeated at both call sites. The per-token throttle maps are why it
// matters: they are keyed by token and are otherwise never pruned, so a missed
// delete would leak one entry per credential ever seen, for the lifetime of the
// plugin instance.
//
// The registry entry is already gone by the time this runs, and every store
// method in internal/pool no-ops for an unknown token, so a usage fetch that is
// still in flight for this credential cannot resurrect it.
func (s *Service) forgetCredential(token string) int {
	if token == "" {
		return 0
	}

	released := s.sessions.DropToken(token)

	s.queueMu.Lock()
	delete(s.inflight, token)
	delete(s.lastAttempt, token)
	s.queueMu.Unlock()

	s.suspectMu.Lock()
	delete(s.lastSuspect, token)
	s.suspectMu.Unlock()

	return released
}

// rebuildSettings resolves exclude_keys and pin_sessions against the live
// credential table and publishes an immutable settings snapshot.
func (s *Service) rebuildSettings() {
	cfg := s.currentConfig()
	states := s.registry.Snapshot()

	// Resolve exclude_keys one reference at a time so a typo can be reported.
	// The reference itself is never logged: it may be a full api-key.
	exclude := make(map[string]struct{})
	for index, reference := range cfg.ExcludeKeys {
		if strings.TrimSpace(reference) == "" {
			continue
		}
		matched := false
		for _, state := range states {
			if pluginconfig.ReferenceMatches(reference, state.Token, state.AuthID, state.APIKey) {
				exclude[state.Token] = struct{}{}
				matched = true
			}
		}
		if !matched {
			s.logIfEnabled(hostbridge.LevelWarn, "exclude_keys entry does not match any credential", map[string]any{
				"index": index,
			})
		}
	}

	pins := make(map[string]string, len(cfg.PinSessions))
	for session, ref := range cfg.PinSessions {
		trimmedSession := strings.TrimSpace(session)
		if trimmedSession == "" {
			continue
		}
		resolved := ""
		for _, state := range states {
			if pluginconfig.ReferenceMatches(ref, state.Token, state.AuthID, state.APIKey) {
				resolved = state.Token
				break
			}
		}
		if resolved == "" {
			s.logIfEnabled(hostbridge.LevelWarn, "pin_sessions entry does not match any credential", map[string]any{
				"session": trimmedSession,
			})
			continue
		}
		pins[trimmedSession] = resolved
	}

	s.settings.Store(&pool.Settings{
		BaseURLPrefix: cfg.BaseURLPrefix,
		StaleAfter:    cfg.StaleAfter.Std(),
		ExcludeTokens: exclude,
		PinTokens:     pins,
	})
}

// routingReady reports whether the credential table can be trusted right now.
func (s *Service) routingReady() (bool, []string) {
	var reasons []string
	if problems := s.currentProblems(); len(problems) > 0 {
		reasons = append(reasons, problems...)
	}
	if !s.configHealthy.Load() {
		reasons = append(reasons, "config_path has not been read successfully")
	}
	return len(reasons) == 0, reasons
}
