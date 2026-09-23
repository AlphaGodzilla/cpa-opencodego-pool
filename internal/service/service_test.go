package service

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"opencodego-pool/internal/hostbridge"
	"opencodego-pool/internal/keysource"
	"opencodego-pool/internal/pluginconfig"
	"opencodego-pool/internal/pool"
)

const (
	// Mirrors a real deployment: an underscore in the provider name and a base-url
	// that already carries the /chat/completions path.
	testProvider  = "openai-compatible-opencode_go"
	testBaseURL   = "https://opencode.ai/zen/go/v1/chat/completions"
	testPrefix    = "https://opencode.ai"
	testUsagePath = "/usage"
)

// usageStub stands in for opencode's usage endpoint. It also asserts the request
// shape, so a regression in the Authorization/Accept/User-Agent headers fails
// the test rather than silently producing empty data.
type usageStub struct {
	mu       sync.Mutex
	rolling  map[string]float64
	weekly   map[string]string
	httpCode map[string]int
	calls    map[string]int
}

func newUsageStub() *usageStub {
	return &usageStub{
		rolling:  map[string]float64{},
		weekly:   map[string]string{},
		httpCode: map[string]int{},
		calls:    map[string]int{},
	}
}

func (s *usageStub) set(apiKey string, rollingPercent float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rolling[apiKey] = rollingPercent
}

func (s *usageStub) setWeeklyStatus(apiKey, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.weekly[apiKey] = status
}

func (s *usageStub) setHTTPStatus(apiKey string, code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.httpCode[apiKey] = code
}

func (s *usageStub) callCount(apiKey string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[apiKey]
}

func (s *usageStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != testUsagePath {
		http.NotFound(w, r)
		return
	}
	apiKey := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

	s.mu.Lock()
	s.calls[apiKey]++
	rolling, known := s.rolling[apiKey]
	weekly := s.weekly[apiKey]
	code := s.httpCode[apiKey]
	s.mu.Unlock()

	if r.Header.Get("Accept") != "application/json" {
		http.Error(w, "missing accept header", http.StatusBadRequest)
		return
	}
	if r.Header.Get("User-Agent") != "opencode-go-quota" {
		http.Error(w, "missing user agent", http.StatusBadRequest)
		return
	}
	if code != 0 {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"error":"rejected"}`))
		return
	}
	if !known {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unknown key"}`))
		return
	}
	if weekly == "" {
		weekly = "ok"
	}
	body := fmt.Sprintf(
		`{"usage":{"rolling":{"status":"ok","percent":%v,"resetsAt":"2026-09-23T14:39:09.347Z"},`+
			`"weekly":{"status":%q,"percent":0},"monthly":{"status":"ok","percent":0}}}`,
		rolling, weekly)
	_, _ = w.Write([]byte(body))
}

// fakeHost implements the plugin→host callbacks this plugin uses.
type fakeHost struct {
	client *http.Client
	mu     sync.Mutex
	logs   []string
}

func (f *fakeHost) call(method string, payload []byte) ([]byte, error) {
	switch method {
	case hostbridge.MethodHostLog:
		var request struct {
			Level   string `json:"level"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(payload, &request)
		f.mu.Lock()
		f.logs = append(f.logs, request.Level+": "+request.Message)
		f.mu.Unlock()
		return envelope(map[string]any{})

	case hostbridge.MethodHostHTTPDo:
		var request struct {
			Method  string      `json:"method"`
			URL     string      `json:"url"`
			Headers http.Header `json:"headers"`
		}
		if errUnmarshal := json.Unmarshal(payload, &request); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		httpRequest, errRequest := http.NewRequest(request.Method, request.URL, nil)
		if errRequest != nil {
			return nil, errRequest
		}
		httpRequest.Header = request.Headers
		response, errDo := f.client.Do(httpRequest)
		if errDo != nil {
			return nil, errDo
		}
		defer func() { _ = response.Body.Close() }()
		body, errRead := io.ReadAll(response.Body)
		if errRead != nil {
			return nil, errRead
		}
		return envelope(map[string]any{
			"StatusCode": response.StatusCode,
			"Headers":    response.Header,
			"Body":       body,
		})
	}
	return nil, fmt.Errorf("unexpected host method %s", method)
}

func (f *fakeHost) logged() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.logs...)
}

func envelope(value any) ([]byte, error) {
	result, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(hostbridge.Envelope{OK: true, Result: result})
}

type fixture struct {
	svc        *Service
	stub       *usageStub
	configPath string
	host       *fakeHost
}

func hostConfig(apiKeys ...string) string {
	var builder strings.Builder
	builder.WriteString("openai-compatibility:\n")
	builder.WriteString("  - name: opencode_go\n")
	builder.WriteString("    base-url: " + testBaseURL + "\n")
	builder.WriteString("    api-key-entries:\n")
	for _, apiKey := range apiKeys {
		builder.WriteString("      - api-key: " + apiKey + "\n")
	}
	return builder.String()
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if errWrite := os.WriteFile(path, []byte(content), 0o600); errWrite != nil {
		t.Fatalf("write %s: %v", path, errWrite)
	}
}

// registerPayload builds the plugin.register request the way the host does: the
// plugin's own config section base64-encoded under config_yaml, next to the RPC
// schema version the host announces.
//
// SchemaVersion is this plugin's maximum, not necessarily the host's: the host
// announces its own and rejects a plugin that answers with a higher one.
func registerPayload(pluginConfig string) []byte {
	return registerPayloadWithSchema(pluginConfig, SchemaVersion)
}

func registerPayloadWithSchema(pluginConfig string, hostSchema uint32) []byte {
	raw, errMarshal := json.Marshal(map[string]any{
		"config_yaml":    []byte(pluginConfig),
		"schema_version": hostSchema,
	})
	if errMarshal != nil {
		panic(errMarshal)
	}
	return raw
}

// TestRegistrationEchoesTheHostSchemaVersion pins the negotiation rule.
//
// The host rejects any plugin declaring a schema version above its own, and that
// rejection never reaches the plugin: plugin.register still answers OK and the only
// symptom is registered=false in the management API. A hardcoded version made this
// plugin fail to load on a v7.2.150 host (schema 5) while every local check passed.
func TestRegistrationEchoesTheHostSchemaVersion(t *testing.T) {
	cases := []struct {
		name string
		host uint32
		want uint32
	}{
		{"host omits the version", 0, 1},
		{"host on the original contract", 1, 1},
		{"host older than this build", 5, 5},
		{"host on our contract", SchemaVersion, SchemaVersion},
		{"host newer than this build", SchemaVersion + 3, SchemaVersion},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			svc := New("opencodego-pool")
			t.Cleanup(svc.Shutdown)
			if _, errRegister := svc.Register(registerPayloadWithSchema("", testCase.host)); errRegister != nil {
				t.Fatalf("register: %v", errRegister)
			}
			if got := svc.registration().SchemaVersion; got != testCase.want {
				t.Fatalf("schema_version = %d, want %d (the host would refuse anything higher)", got, testCase.want)
			}
		})
	}
}

func newFixture(t *testing.T, host string) *fixture {
	t.Helper()

	stub := newUsageStub()
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, configPath, host)

	hostStub := &fakeHost{client: server.Client()}
	hostbridge.Install(hostStub.call)
	t.Cleanup(func() { hostbridge.Install(nil) })

	pluginYAML := fmt.Sprintf(
		"usage_url: %q\nconfig_path: %q\npoll_interval: 1h\nrequest_timeout: 5s\nsession_ttl: 1h\nlog_level: debug\n",
		server.URL+testUsagePath, configPath)

	svc := New("opencodego-pool")
	if _, errRegister := svc.Register(registerPayload(pluginYAML)); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	t.Cleanup(svc.Shutdown)

	return &fixture{svc: svc, stub: stub, configPath: configPath, host: hostStub}
}

func (f *fixture) keys(t *testing.T) []keysource.Key {
	t.Helper()
	keys, errLoad := keysource.LoadKeys(f.configPath, testPrefix)
	if errLoad != nil {
		t.Fatalf("LoadKeys: %v", errLoad)
	}
	return keys
}

func (f *fixture) tokenFor(t *testing.T, apiKey string) string {
	t.Helper()
	for _, key := range f.keys(t) {
		if key.APIKey == apiKey {
			return key.Token
		}
	}
	t.Fatalf("no token for %s", apiKey)
	return ""
}

func (f *fixture) authIDFor(t *testing.T, apiKey string) string {
	t.Helper()
	for _, key := range f.keys(t) {
		if key.APIKey == apiKey {
			return key.AuthID
		}
	}
	t.Fatalf("no auth id for %s", apiKey)
	return ""
}

func (f *fixture) apiKeyOf(t *testing.T, authID string) string {
	t.Helper()
	index := strings.LastIndexByte(authID, ':')
	if index < 0 {
		t.Fatalf("malformed auth id %q", authID)
	}
	token := authID[index+1:]
	for _, key := range f.keys(t) {
		if key.Token == token {
			return key.APIKey
		}
	}
	t.Fatalf("no credential for auth id %q", authID)
	return ""
}

func (f *fixture) awaitUsage(t *testing.T, apiKey string) {
	t.Helper()
	token := f.tokenFor(t, apiKey)
	await(t, "usage for "+apiKey, func() bool {
		state, ok := f.svc.registry.Get(token)
		return ok && state.HasUsage
	})
}

func (f *fixture) awaitExcluded(t *testing.T, apiKey string) {
	t.Helper()
	token := f.tokenFor(t, apiKey)
	await(t, "credential "+apiKey+" to be excluded", func() bool {
		state, ok := f.svc.registry.Get(token)
		return ok && f.svc.selector.ExclusionReason(state, f.svc.currentSettings(), time.Now()) != ""
	})
}

func await(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (f *fixture) pick(t *testing.T, session string, apiKeys ...string) pluginapi.SchedulerPickResponse {
	t.Helper()
	candidates := make([]pluginapi.SchedulerAuthCandidate, 0, len(apiKeys))
	for _, apiKey := range apiKeys {
		token := f.tokenFor(t, apiKey)
		candidates = append(candidates, pluginapi.SchedulerAuthCandidate{
			ID:       "openai-compatibility:opencodego:" + token,
			Provider: testProvider,
			Attributes: map[string]string{
				"base_url": testBaseURL,
				"source":   "config:opencodego[" + token + "]",
			},
		})
	}
	request := pluginapi.SchedulerPickRequest{Provider: testProvider, Candidates: candidates}
	if session != "" {
		request.Options = pluginapi.SchedulerOptions{
			Headers: map[string][]string{"X-Opencode-Session": {session}},
		}
	}
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("marshal pick request: %v", errMarshal)
	}
	response, errPick := f.svc.Pick(raw)
	if errPick != nil {
		t.Fatalf("pick: %v", errPick)
	}
	return response
}

// pickAuthIDs issues a pick from pre-computed auth ids, so a test can still
// exercise routing after config.yaml has been made unreadable.
func (f *fixture) pickAuthIDs(t *testing.T, session string, authIDs ...string) pluginapi.SchedulerPickResponse {
	t.Helper()
	candidates := make([]pluginapi.SchedulerAuthCandidate, 0, len(authIDs))
	for _, authID := range authIDs {
		index := strings.LastIndexByte(authID, ':')
		if index < 0 {
			t.Fatalf("malformed auth id %q", authID)
		}
		token := authID[index+1:]
		candidates = append(candidates, pluginapi.SchedulerAuthCandidate{
			ID:       authID,
			Provider: testProvider,
			Attributes: map[string]string{
				"base_url": testBaseURL,
				"source":   "config:opencodego[" + token + "]",
			},
		})
	}
	request := pluginapi.SchedulerPickRequest{Provider: testProvider, Candidates: candidates}
	if session != "" {
		request.Options = pluginapi.SchedulerOptions{
			Headers: map[string][]string{"X-Opencode-Session": {session}},
		}
	}
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("marshal pick request: %v", errMarshal)
	}
	response, errPick := f.svc.Pick(raw)
	if errPick != nil {
		t.Fatalf("pick: %v", errPick)
	}
	return response
}

func TestServiceRoutesNewSessionsToTheLeastUsedKey(t *testing.T) {
	f := newFixture(t, hostConfig("sk-one", "sk-two", "sk-three"))
	f.stub.set("sk-one", 10)
	f.stub.set("sk-two", 50)
	f.stub.set("sk-three", 90)
	for _, apiKey := range []string{"sk-one", "sk-two", "sk-three"} {
		f.awaitUsage(t, apiKey)
	}

	response := f.pick(t, "session-a", "sk-one", "sk-two", "sk-three")
	if !response.Handled {
		t.Fatalf("response = %+v, want handled", response)
	}
	if got := f.apiKeyOf(t, response.AuthID); got != "sk-one" {
		t.Fatalf("routed to %s, want sk-one", got)
	}
}

func TestServiceKeepsSessionsSticky(t *testing.T) {
	f := newFixture(t, hostConfig("sk-one", "sk-two"))
	f.stub.set("sk-one", 10)
	f.stub.set("sk-two", 90)
	f.awaitUsage(t, "sk-one")
	f.awaitUsage(t, "sk-two")

	first := f.pick(t, "session-a", "sk-one", "sk-two")
	if got := f.apiKeyOf(t, first.AuthID); got != "sk-one" {
		t.Fatalf("first = %s, want sk-one", got)
	}

	// sk-two becomes the emptiest, but the session must not move.
	f.stub.set("sk-two", 0)
	f.svc.requestRefresh(f.tokenFor(t, "sk-two"), true)
	await(t, "sk-two to be refreshed", func() bool {
		state, ok := f.svc.registry.Get(f.tokenFor(t, "sk-two"))
		return ok && state.HasUsage && state.Usage.Rolling.Percent == 0
	})

	second := f.pick(t, "session-a", "sk-one", "sk-two")
	if got := f.apiKeyOf(t, second.AuthID); got != "sk-one" {
		t.Fatalf("second = %s, want sk-one (sticky)", got)
	}
}

func TestServiceRebindsWhenTheBoundKeyRunsOut(t *testing.T) {
	f := newFixture(t, hostConfig("sk-one", "sk-two"))
	f.stub.set("sk-one", 0)
	f.stub.set("sk-two", 90)
	f.awaitUsage(t, "sk-one")
	f.awaitUsage(t, "sk-two")

	if got := f.apiKeyOf(t, f.pick(t, "session-a", "sk-one", "sk-two").AuthID); got != "sk-one" {
		t.Fatalf("initial route = %s, want sk-one", got)
	}

	f.stub.setWeeklyStatus("sk-one", "rate-limited")
	f.svc.requestRefresh(f.tokenFor(t, "sk-one"), true)
	f.awaitExcluded(t, "sk-one")

	response := f.pick(t, "session-a", "sk-one", "sk-two")
	if got := f.apiKeyOf(t, response.AuthID); got != "sk-two" {
		t.Fatalf("after exhaustion routed to %s, want sk-two", got)
	}
}

func TestServiceDiscoversAnAddedKeyWithoutARestart(t *testing.T) {
	f := newFixture(t, hostConfig("sk-one"))
	f.stub.set("sk-one", 80)
	f.awaitUsage(t, "sk-one")

	// A fresh key appears in config.yaml, exactly as a manual edit would do.
	writeFile(t, f.configPath, hostConfig("sk-one", "sk-fresh"))
	f.stub.set("sk-fresh", 0)
	f.svc.syncConfigFile(true)

	f.awaitUsage(t, "sk-fresh")
	if token := f.tokenFor(t, "sk-fresh"); !f.svc.registry.IsTracked(token) {
		t.Fatal("the new credential should be tracked")
	}

	response := f.pick(t, "session-new", "sk-one", "sk-fresh")
	if got := f.apiKeyOf(t, response.AuthID); got != "sk-fresh" {
		t.Fatalf("routed to %s, want sk-fresh", got)
	}
}

func TestServiceDropsARemovedKeyAndItsBindings(t *testing.T) {
	f := newFixture(t, hostConfig("sk-one", "sk-two"))
	f.stub.set("sk-one", 0)
	f.stub.set("sk-two", 50)
	f.awaitUsage(t, "sk-one")
	f.awaitUsage(t, "sk-two")

	if got := f.apiKeyOf(t, f.pick(t, "session-a", "sk-one", "sk-two").AuthID); got != "sk-one" {
		t.Fatalf("initial route = %s, want sk-one", got)
	}
	removedToken := f.tokenFor(t, "sk-one")

	writeFile(t, f.configPath, hostConfig("sk-two"))
	f.svc.syncConfigFile(true)

	if f.svc.registry.IsTracked(removedToken) {
		t.Fatal("the removed credential should be gone")
	}
	if count := f.svc.sessions.CountToken(removedToken); count != 0 {
		t.Fatalf("bindings on the removed credential = %d, want 0", count)
	}
	if got := f.apiKeyOf(t, f.pick(t, "session-a", "sk-two").AuthID); got != "sk-two" {
		t.Fatalf("the session should have been released to sk-two, got %s", got)
	}
}

func TestServiceDeclinesWhileConfigIsUnreadable(t *testing.T) {
	f := newFixture(t, hostConfig("sk-one"))
	f.stub.set("sk-one", 0)
	f.awaitUsage(t, "sk-one")
	authID := f.authIDFor(t, "sk-one")
	if !f.pickAuthIDs(t, "session-a", authID).Handled {
		t.Fatal("the plugin should route before the config disappears")
	}

	if errRemove := os.Remove(f.configPath); errRemove != nil {
		t.Fatalf("remove config: %v", errRemove)
	}
	f.svc.syncConfigFile(true)

	// The auth id is supplied directly: the config file is gone, so it can no
	// longer be recovered from there.
	if response := f.pickAuthIDs(t, "session-b", authID); response.Handled {
		t.Fatal("an unreadable config_path must stop routing")
	}
}

func TestServiceDeclinesWhenTheMappingDrifts(t *testing.T) {
	f := newFixture(t, hostConfig("sk-one"))
	f.stub.set("sk-one", 0)
	f.awaitUsage(t, "sk-one")

	// A candidate that belongs to the pool but whose token this plugin cannot
	// explain. Routing must stop rather than pick something arbitrary.
	request := pluginapi.SchedulerPickRequest{
		Provider: testProvider,
		Candidates: []pluginapi.SchedulerAuthCandidate{{
			ID:       "openai-compatibility:opencodego:deadbeefcafe",
			Provider: testProvider,
			Attributes: map[string]string{
				"base_url": testBaseURL,
				"source":   "config:opencodego[deadbeefcafe]",
			},
		}},
		Options: pluginapi.SchedulerOptions{Headers: map[string][]string{"X-Opencode-Session": {"session-a"}}},
	}
	raw, _ := json.Marshal(request)

	response, errPick := f.svc.Pick(raw)
	if errPick != nil {
		t.Fatalf("pick: %v", errPick)
	}
	if response.Handled {
		t.Fatalf("response = %+v, want a declined pick", response)
	}
	if f.svc.stats.MappingFailed.Load() != 1 {
		t.Errorf("mapping failures = %d, want 1", f.svc.stats.MappingFailed.Load())
	}

	// A following healthy pick must restore routing.
	if !f.pick(t, "session-b", "sk-one").Handled {
		t.Fatal("routing should recover once candidates match again")
	}
}

func TestServiceIgnoresRequestsWithoutASessionHeader(t *testing.T) {
	f := newFixture(t, hostConfig("sk-one"))
	f.stub.set("sk-one", 0)
	f.awaitUsage(t, "sk-one")

	if response := f.pick(t, "", "sk-one"); response.Handled {
		t.Fatalf("response = %+v, want the host scheduler to stay in charge", response)
	}
}

func TestService429FastPathExcludesTheKey(t *testing.T) {
	f := newFixture(t, hostConfig("sk-one", "sk-two"))
	f.stub.set("sk-one", 0)
	f.stub.set("sk-two", 50)
	f.awaitUsage(t, "sk-one")
	f.awaitUsage(t, "sk-two")

	// sk-one is the emptier credential, so only the 429 signal can move traffic off
	// it. Hold its refresh slot first: usage.handle kicks off a usage read, and a
	// successful read is allowed to clear the suspicion (see the test below). This
	// test is about the 429 signal itself, so the read must not race it.
	token := f.tokenFor(t, "sk-one")
	holdRefreshSlot(t, f.svc, token)
	defer releaseRefreshSlot(t, f.svc, token)

	if errHandle := f.svc.HandleUsage(mustMarshalUsageRecord(t, pluginapi.UsageRecord{
		Provider: testProvider,
		BaseURL:  testBaseURL,
		AuthID:   f.authIDFor(t, "sk-one"),
		Failed:   true,
		Failure:  pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests},
	})); errHandle != nil {
		t.Fatalf("HandleUsage: %v", errHandle)
	}

	if f.svc.stats.Suspected429.Load() != 1 {
		t.Errorf("429 counter = %d, want 1", f.svc.stats.Suspected429.Load())
	}
	if reason := exclusionOf(f, token); reason != "recent upstream 429" {
		t.Fatalf("exclusion reason = %q, want the 429 flag", reason)
	}

	response := f.pick(t, "session-new", "sk-one", "sk-two")
	if got := f.apiKeyOf(t, response.AuthID); got != "sk-two" {
		t.Fatalf("routed to %s, want sk-two after the 429", got)
	}
}

// TestService429SuspicionIsHeldForAMinimumWindow pins the rule that a live 429
// outranks the usage endpoint for pool.SuspectHold.
//
// The endpoint can lag behind the request path, so a successful read must not be
// able to erase the 429 evidence immediately — that is exactly the race that made
// the fast path useless before the hold existed. The hold is a floor rather than a
// permanent mark: once it elapses the credential is judged on fresh data again.
func TestService429SuspicionIsHeldForAMinimumWindow(t *testing.T) {
	f := newFixture(t, hostConfig("sk-one"))
	f.stub.set("sk-one", 10)
	f.awaitUsage(t, "sk-one")
	token := f.tokenFor(t, "sk-one")

	f.svc.registry.MarkSuspected429(token, time.Now())
	if reason := exclusionOf(f, token); reason != "recent upstream 429" {
		t.Fatalf("exclusion reason = %q, want the 429 flag", reason)
	}

	// A read that lands inside the hold must leave the suspicion alone.
	f.stub.set("sk-one", 20)
	f.svc.requestRefresh(token, true)
	await(t, "the usage read to land", func() bool {
		state, ok := f.svc.registry.Get(token)
		return ok && state.Usage.Rolling.Percent == 20
	})
	if reason := exclusionOf(f, token); reason != "recent upstream 429" {
		t.Fatalf("a read inside the hold must not clear the suspicion, got %q", reason)
	}

	// The hold expires on its own: no further read is needed for the credential to
	// become usable again.
	state, ok := f.svc.registry.Get(token)
	if !ok {
		t.Fatal("credential missing")
	}
	if state.Suspected429(time.Now().Add(pool.SuspectHold + time.Second)) {
		t.Fatal("the hold should have elapsed by then")
	}

	// And a read past the hold clears the stored flag for good.
	f.svc.registry.StoreUsage(token, state.Usage, time.Now().Add(pool.SuspectHold+time.Second))
	if state, _ := f.svc.registry.Get(token); !state.Suspected429At.IsZero() {
		t.Fatal("a read past the hold should clear the suspicion")
	}
}

// mustMarshalUsageRecord encodes a usage.handle payload.
func mustMarshalUsageRecord(t *testing.T, record pluginapi.UsageRecord) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(record)
	if errMarshal != nil {
		t.Fatalf("marshal usage record: %v", errMarshal)
	}
	return raw
}

// exclusionOf reports why the selector would skip a credential right now.
func exclusionOf(f *fixture, token string) string {
	state, ok := f.svc.registry.Get(token)
	if !ok {
		return "credential missing"
	}
	return f.svc.selector.ExclusionReason(state, f.svc.currentSettings(), time.Now())
}

// holdRefreshSlot makes requestRefresh treat the credential as already being
// refreshed, so a test can control when its usage is re-read.
func holdRefreshSlot(t *testing.T, svc *Service, token string) {
	t.Helper()
	svc.queueMu.Lock()
	defer svc.queueMu.Unlock()
	if _, busy := svc.inflight[token]; busy {
		t.Fatalf("refresh slot for %s was already taken", token)
	}
	svc.inflight[token] = struct{}{}
}

func releaseRefreshSlot(t *testing.T, svc *Service, token string) {
	t.Helper()
	svc.queueMu.Lock()
	defer svc.queueMu.Unlock()
	delete(svc.inflight, token)
}

func TestServiceIgnoresUsageRecordsForOtherProviders(t *testing.T) {
	f := newFixture(t, hostConfig("sk-one"))
	f.stub.set("sk-one", 0)
	f.awaitUsage(t, "sk-one")

	raw, _ := json.Marshal(pluginapi.UsageRecord{
		AuthID:  "some-other-auth-id",
		Failed:  true,
		Failure: pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests},
	})
	if errHandle := f.svc.HandleUsage(raw); errHandle != nil {
		t.Fatalf("HandleUsage: %v", errHandle)
	}
	if f.svc.stats.Suspected429.Load() != 0 {
		t.Error("an unrelated auth id must not be flagged")
	}
}

func TestServiceQuotaFetchServesCachedUsage(t *testing.T) {
	f := newFixture(t, hostConfig("sk-one"))
	f.stub.set("sk-one", 42)
	f.awaitUsage(t, "sk-one")

	request, _ := json.Marshal(pluginapi.QuotaFetchRequest{
		AuthID: f.authIDFor(t, "sk-one"),
	})
	response, errFetch := f.svc.FetchQuota(request)
	if errFetch != nil {
		t.Fatalf("FetchQuota: %v", errFetch)
	}
	if len(response.Groups) != 1 || len(response.Groups[0].Buckets) == 0 {
		t.Fatalf("groups = %+v, want one group with buckets", response.Groups)
	}
	bucket := response.Groups[0].Buckets[0]
	if bucket.Window != "rolling" {
		t.Errorf("window = %q, want rolling", bucket.Window)
	}
	if want := 0.58; bucket.RemainingFraction < want-0.001 || bucket.RemainingFraction > want+0.001 {
		t.Errorf("remaining fraction = %v, want ~%v", bucket.RemainingFraction, want)
	}
}

func TestServiceQuotaFetchRejectsUnknownCredentials(t *testing.T) {
	f := newFixture(t, hostConfig("sk-one"))
	f.stub.set("sk-one", 0)
	f.awaitUsage(t, "sk-one")

	request, _ := json.Marshal(pluginapi.QuotaFetchRequest{AuthID: "unrelated"})
	if _, errFetch := f.svc.FetchQuota(request); errFetch == nil {
		t.Fatal("expected an error for an unrelated credential")
	}
}

func TestServiceManagementStateNeverLeaksAPIKeys(t *testing.T) {
	f := newFixture(t, hostConfig("sk-secret-one", "sk-secret-two"))
	f.stub.set("sk-secret-one", 10)
	f.stub.set("sk-secret-two", 20)
	f.awaitUsage(t, "sk-secret-one")
	f.awaitUsage(t, "sk-secret-two")
	f.pick(t, "session-a", "sk-secret-one", "sk-secret-two")

	body, errMarshal := json.Marshal(f.svc.StateSnapshot())
	if errMarshal != nil {
		t.Fatalf("marshal snapshot: %v", errMarshal)
	}
	text := string(body)
	for _, secret := range []string{"sk-secret-one", "sk-secret-two"} {
		if strings.Contains(text, secret) {
			t.Fatalf("the snapshot leaked %s", secret)
		}
	}
	if !strings.Contains(text, "rolling") {
		t.Error("the snapshot should carry usage data")
	}
}

func TestServiceManagementHandlesBothRoutes(t *testing.T) {
	f := newFixture(t, hostConfig("sk-one"))
	f.stub.set("sk-one", 0)
	f.awaitUsage(t, "sk-one")

	registration := f.svc.RegisterManagement()
	if len(registration.Routes) != 1 || len(registration.Resources) != 1 {
		t.Fatalf("registration = %+v", registration)
	}

	stateRequest, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   registration.Routes[0].Path,
	})
	state, errState := f.svc.HandleManagement(stateRequest)
	if errState != nil {
		t.Fatalf("management state: %v", errState)
	}
	if state.StatusCode != http.StatusOK || !json.Valid(state.Body) {
		t.Fatalf("state response = %d %s", state.StatusCode, state.Body)
	}

	resourceRequest, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/" + f.svc.PluginID() + registration.Resources[0].Path,
	})
	page, errPage := f.svc.HandleManagement(resourceRequest)
	if errPage != nil {
		t.Fatalf("resource page: %v", errPage)
	}
	if page.StatusCode != http.StatusOK || !strings.Contains(string(page.Body), "OpenCode Pool") {
		t.Fatalf("page response = %d %s", page.StatusCode, page.Body)
	}

	unknownRequest, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/nope"})
	unknown, _ := f.svc.HandleManagement(unknownRequest)
	if unknown.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown route = %d, want 404", unknown.StatusCode)
	}
}

func TestServiceRegistersWithoutConfigPath(t *testing.T) {
	hostbridge.Install(func(method string, payload []byte) ([]byte, error) {
		return envelope(map[string]any{})
	})
	t.Cleanup(func() { hostbridge.Install(nil) })

	svc := New("opencodego-pool")
	t.Cleanup(svc.Shutdown)

	registration, errRegister := svc.Register(registerPayload("usage_url: \"\"\n"))
	if errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	if registration.SchemaVersion != SchemaVersion {
		t.Errorf("schema version = %d, want %d", registration.SchemaVersion, SchemaVersion)
	}
	if !registration.Capabilities.Scheduler || !registration.Capabilities.UsagePlugin ||
		!registration.Capabilities.ManagementAPI || !registration.Capabilities.QuotaProvider {
		t.Errorf("capabilities = %+v", registration.Capabilities)
	}

	request := pluginapi.SchedulerPickRequest{
		Provider: testProvider,
		Candidates: []pluginapi.SchedulerAuthCandidate{{
			ID:         "openai-compatibility:opencodego:deadbeefcafe",
			Attributes: map[string]string{"base_url": testBaseURL, "source": "config:opencodego[deadbeefcafe]"},
		}},
		Options: pluginapi.SchedulerOptions{Headers: map[string][]string{"X-Opencode-Session": {"s"}}},
	}
	raw, _ := json.Marshal(request)
	response, errPick := svc.Pick(raw)
	if errPick != nil {
		t.Fatalf("pick: %v", errPick)
	}
	if response.Handled {
		t.Fatal("a plugin without config_path must decline every pick")
	}
}

func TestServiceReconfigureIsFastAndKeepsRouting(t *testing.T) {
	f := newFixture(t, hostConfig("sk-one"))
	f.stub.set("sk-one", 0)
	f.awaitUsage(t, "sk-one")

	pluginYAML := fmt.Sprintf(
		"usage_url: %q\nconfig_path: %q\npoll_interval: 5m\nsession_ttl: 30m\n",
		"http://127.0.0.1:1"+testUsagePath, f.configPath)

	started := time.Now()
	if _, errReconfigure := f.svc.Reconfigure(registerPayload(pluginYAML)); errReconfigure != nil {
		t.Fatalf("reconfigure: %v", errReconfigure)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("reconfigure took %v; it runs ahead of the credential diff and must return immediately", elapsed)
	}

	// The new TTL takes effect for bindings created afterwards.
	await(t, "the reconfigured poll interval", func() bool {
		return f.svc.pollInterval() == 5*time.Minute
	})
}

// TestRegistrationSatisfiesHostValidation mirrors the host's validPlugin().
//
// An empty Name, Version, Author or GitHubRepository makes the host accept the
// plugin.register RPC and then silently refuse to register the plugin, so the only
// symptom is registered=false in the management API with nothing in the plugin's
// own logs. That failure mode is invisible from inside the plugin, which is
// exactly why it has to be asserted here.
func TestRegistrationSatisfiesHostValidation(t *testing.T) {
	svc := New("opencodego-pool")
	t.Cleanup(svc.Shutdown)
	if _, errRegister := svc.Register(registerPayload("")); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	registration := svc.registration()

	for _, field := range []struct{ name, value string }{
		{"metadata.Name", registration.Metadata.Name},
		{"metadata.Version", registration.Metadata.Version},
		{"metadata.Author", registration.Metadata.Author},
		{"metadata.GitHubRepository", registration.Metadata.GitHubRepository},
	} {
		if strings.TrimSpace(field.value) == "" {
			t.Errorf("%s must not be empty: validPlugin() would reject the registration", field.name)
		}
	}
	if !registration.Capabilities.Scheduler {
		t.Error("the host requires at least one declared capability")
	}
}

// TestMetadataFieldsAreConfigurable pins that a user-supplied author and
// repository reach the registration instead of being overwritten by the defaults.
func TestMetadataFieldsAreConfigurable(t *testing.T) {
	svc := New("opencodego-pool")
	t.Cleanup(svc.Shutdown)

	const repository = "https://github.com/me/cpa_opencodego_pool"
	config := "author: \"me\"\nrepository: \"" + repository + "\"\n"
	if _, errRegister := svc.Register(registerPayload(config)); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}

	registration := svc.registration()
	if registration.Metadata.Author != "me" {
		t.Errorf("Author = %q, want me", registration.Metadata.Author)
	}
	if registration.Metadata.GitHubRepository != repository {
		t.Errorf("GitHubRepository = %q, want %q", registration.Metadata.GitHubRepository, repository)
	}
}

// tracksThrottleBookkeeping reports whether any per-token bookkeeping still
// mentions the credential.
//
// Unlike the credential table and the binding table, these maps have no natural
// bound: their keys come from whatever tokens the plugin has ever seen, so a
// missed delete on removal is a leak that grows with key rotation.
func tracksThrottleBookkeeping(t *testing.T, svc *Service, token string) bool {
	t.Helper()

	svc.queueMu.Lock()
	_, inFlight := svc.inflight[token]
	_, attempted := svc.lastAttempt[token]
	svc.queueMu.Unlock()

	svc.suspectMu.Lock()
	_, suspected := svc.lastSuspect[token]
	svc.suspectMu.Unlock()

	return inFlight || attempted || suspected
}

func TestServiceForgetsRemovedCredentialBookkeeping(t *testing.T) {
	f := newFixture(t, hostConfig("sk-one", "sk-two"))
	f.stub.set("sk-one", 0)
	f.stub.set("sk-two", 50)
	f.awaitUsage(t, "sk-one")
	f.awaitUsage(t, "sk-two")

	// The token has to be read while config.yaml still contains the credential.
	removedToken := f.tokenFor(t, "sk-one")
	removedAuthID := f.authIDFor(t, "sk-one")

	// Populate the maps: one queued refresh and one 429 fast-path hit.
	f.svc.requestRefresh(removedToken, true)
	raw, errMarshal := json.Marshal(pluginapi.UsageRecord{
		AuthID:  removedAuthID,
		Failed:  true,
		Failure: pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests},
	})
	if errMarshal != nil {
		t.Fatalf("marshal usage record: %v", errMarshal)
	}
	if errHandle := f.svc.HandleUsage(raw); errHandle != nil {
		t.Fatalf("HandleUsage: %v", errHandle)
	}

	// Let the queued fetch drain, so a surviving inflight entry would mean a real
	// leak rather than a request that simply had not run yet.
	await(t, "the queued refresh to drain", func() bool {
		f.svc.queueMu.Lock()
		defer f.svc.queueMu.Unlock()
		_, busy := f.svc.inflight[removedToken]
		return !busy
	})
	if !tracksThrottleBookkeeping(t, f.svc, removedToken) {
		t.Fatal("the fixture did not populate the per-token bookkeeping")
	}

	writeFile(t, f.configPath, hostConfig("sk-two"))
	f.svc.syncConfigFile(true)

	if f.svc.registry.IsTracked(removedToken) {
		t.Fatal("the credential should be gone from the table")
	}
	if tracksThrottleBookkeeping(t, f.svc, removedToken) {
		t.Fatal("removing a credential must also drop its throttle bookkeeping")
	}
}

func TestForgetCredentialClearsEveryTrace(t *testing.T) {
	svc := New("opencodego-pool")
	t.Cleanup(svc.Shutdown)

	svc.sessions.Bind("session-a", "provider-x", "tok-a")
	svc.sessions.Bind("session-b", "provider-x", "tok-a")
	svc.sessions.Bind("session-c", "provider-x", "tok-b")

	svc.queueMu.Lock()
	svc.inflight["tok-a"] = struct{}{}
	svc.lastAttempt["tok-a"] = time.Now()
	svc.queueMu.Unlock()
	svc.suspectMu.Lock()
	svc.lastSuspect["tok-a"] = time.Now()
	svc.suspectMu.Unlock()

	if released := svc.forgetCredential("tok-a"); released != 2 {
		t.Fatalf("released %d bindings, want 2", released)
	}
	if svc.sessions.CountToken("tok-a") != 0 {
		t.Error("bindings on the forgotten credential should be gone")
	}
	if svc.sessions.CountToken("tok-b") != 1 {
		t.Error("unrelated bindings must survive")
	}
	if tracksThrottleBookkeeping(t, svc, "tok-a") {
		t.Error("throttle bookkeeping should be gone")
	}
	if released := svc.forgetCredential(""); released != 0 {
		t.Errorf("forgetting an empty token should be a no-op, got %d", released)
	}
}

func TestSweepForgetsPrunedCredentialBookkeeping(t *testing.T) {
	svc := New("opencodego-pool")
	t.Cleanup(svc.Shutdown)

	// A short poll interval keeps the prune window small, and the credential is
	// marked as last seen an hour ago, so nothing here needs to sleep.
	svc.applyConfig(pluginconfig.Config{
		UsageURL:     "https://example.invalid/usage",
		ConfigPath:   filepath.Join(t.TempDir(), "config.yaml"),
		PollInterval: pluginconfig.Duration(time.Second),
	})

	svc.registry.Sync([]keysource.Key{{
		Token:    "tok-a",
		Name:     "opencode_go",
		Provider: testProvider,
		AuthID:   "openai-compatibility:opencode_go:tok-a",
		APIKey:   "sk-hidden",
		BaseURL:  testBaseURL,
	}})
	svc.registry.Touch("tok-a", time.Now().Add(-time.Hour))
	for i := 0; i < pruneFailStreak; i++ {
		svc.registry.StoreFetchFailure("tok-a")
	}

	svc.queueMu.Lock()
	svc.lastAttempt["tok-a"] = time.Now()
	svc.queueMu.Unlock()
	svc.suspectMu.Lock()
	svc.lastSuspect["tok-a"] = time.Now()
	svc.suspectMu.Unlock()

	svc.sweep()

	if svc.registry.IsTracked("tok-a") {
		t.Fatal("a credential that never appears as a candidate and keeps failing must be pruned")
	}
	if tracksThrottleBookkeeping(t, svc, "tok-a") {
		t.Fatal("the prune path must also drop the throttle bookkeeping")
	}
}
