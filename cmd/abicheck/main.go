// Command abicheck loads the built plugin the way CLIProxyAPI does and drives it
// through the C ABI: init, register, a scheduler pick, a quota fetch, shutdown.
//
// The unit and integration tests cover the Go side of the plugin, but they all
// run in-process with a fake host bridge. This is the only check that the
// exported symbols, the function tables, and the JSON envelopes actually match
// what the host expects — the part that would otherwise fail silently on load.
//
// Run it through `make abicheck`.
package main

/*
// dlopen lives in libSystem on macOS, so only Linux needs the explicit link.
#cgo linux LDFLAGS: -ldl
#cgo freebsd LDFLAGS: -ldl

// A file carrying //export has its preamble compiled twice, so the preamble may
// only declare things. Every definition lives in bridge.c.
#include "bridge.h"
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"opencodego-pool/internal/keysource"
)

const (
	abiVersion    = 1
	schemaVersion = 6
	// The provider block below mirrors a real opencode deployment: an underscore in
	// the name, a base-url carrying the /chat/completions path, and a usage endpoint
	// that must be configured explicitly because it cannot be derived from it.
	providerKey    = "openai-compatible-opencode_go"
	providerName   = "opencode_go"
	providerBase   = "https://opencode.ai/zen/go/v1/chat/completions"
	apiKey         = "oc_sk_abi_01"
	usageURL       = "https://opencode.ai/zen/go/v1/usage"
	usagePercent   = 7.0
	sessionHeader  = "X-Opencode-Session"
	settleDuration = 1500 * time.Millisecond
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: abicheck <path-to-plugin.dylib>")
		os.Exit(2)
	}
	if errRun := run(os.Args[1]); errRun != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", errRun)
		os.Exit(1)
	}
	fmt.Println("OK: the plugin loaded, registered, and answered through the C ABI")
}

func run(path string) error {
	configPath, token, errSetup := setupWorkspace()
	if errSetup != nil {
		return errSetup
	}
	authID := keysource.AuthID(keysource.KindForProvider(providerName), token)

	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))

	handle := C.abicheckOpen(cPath)
	if handle == nil {
		return fmt.Errorf("dlopen %s: %s", path, C.GoString(C.abicheckError()))
	}
	defer C.abicheckClose(handle)

	cSymbolName := C.CString("cliproxy_plugin_init")
	defer C.free(unsafe.Pointer(cSymbolName))

	initSymbol := C.abicheckSymbol(handle, cSymbolName)
	if initSymbol == nil {
		return fmt.Errorf("the shared object does not export cliproxy_plugin_init")
	}

	host := C.cliproxy_host_api{
		abi_version: C.uint32_t(abiVersion),
		host_ctx:    nil,
		call:        C.cliproxy_host_call_fn(C.abicheckHostCallBridge),
		free_buffer: C.cliproxy_host_free_fn(C.abicheckHostFreeBridge),
	}

	var plugin C.cliproxy_plugin_api
	if rc := C.abicheckInit(initSymbol, &host, &plugin); rc != 0 {
		return fmt.Errorf("cliproxy_plugin_init returned %d", int(rc))
	}
	if plugin.call == nil || plugin.free_buffer == nil || plugin.shutdown == nil {
		return fmt.Errorf("cliproxy_plugin_init left a function-table entry nil")
	}
	if got := uint32(plugin.abi_version); got != abiVersion {
		return fmt.Errorf("abi_version = %d, want %d", got, abiVersion)
	}
	fmt.Println("step 1/5  cliproxy_plugin_init filled the plugin table")

	if errRegister := checkRegistration(&plugin, configPath); errRegister != nil {
		return errRegister
	}

	// New credentials are fetched as soon as they are discovered, so a short wait
	// is enough for host.http.do to have been exercised.
	time.Sleep(settleDuration)

	if errQuota := checkQuota(&plugin, authID); errQuota != nil {
		return errQuota
	}
	if errPick := checkPick(&plugin, authID); errPick != nil {
		return errPick
	}

	C.abicheckShutdownPlugin(plugin.shutdown)
	fmt.Println("step 5/5  cliproxy_plugin_shutdown returned cleanly")
	return nil
}

// setupWorkspace writes a minimal CLIProxyAPI config.yaml and returns its path
// along with the token the host would derive for the single api-key in it.
func setupWorkspace() (string, string, error) {
	dir, errDir := os.MkdirTemp("", "opencodego-abicheck-")
	if errDir != nil {
		return "", "", errDir
	}
	configPath := filepath.Join(dir, "config.yaml")
	// YAML forbids tabs for indentation, so the literal below uses spaces.
	config := fmt.Sprintf("openai-compatibility:\n"+
		"  - name: %s\n"+
		"    base-url: %s\n"+
		"    disable-cooling: true\n"+
		"    api-key-entries:\n"+
		"      - api-key: %s\n"+
		"        weight: 1\n"+
		"    models:\n"+
		"      - name: deepseek-v4.1-flash\n"+
		"        alias: \"\"\n",
		providerName, providerBase, apiKey)
	if errWrite := os.WriteFile(configPath, []byte(config), 0o600); errWrite != nil {
		return "", "", errWrite
	}
	keys, errLoad := keysource.LoadKeys(configPath, providerBase)
	if errLoad != nil {
		return "", "", errLoad
	}
	if len(keys) != 1 {
		return "", "", fmt.Errorf("expected exactly one credential, got %d", len(keys))
	}
	return configPath, keys[0].Token, nil
}

// registrationShape mirrors the host's rpcRegistration plus the metadata fields
// its validPlugin() insists on.
type registrationShape struct {
	SchemaVersion uint32 `json:"schema_version"`
	Metadata      struct {
		Name             string `json:"Name"`
		Version          string `json:"Version"`
		Author           string `json:"Author"`
		GitHubRepository string `json:"GitHubRepository"`
	} `json:"metadata"`
	Capabilities struct {
		Scheduler     bool `json:"scheduler"`
		UsagePlugin   bool `json:"usage_plugin"`
		ManagementAPI bool `json:"management_api"`
		QuotaProvider bool `json:"quota_provider"`
	} `json:"capabilities"`
}

// hostSchemaVersions are the contract versions this check registers against.
//
// 5 is not arbitrary: CLIProxyAPI v7.2.150 announces schema 5, and the host rejects
// any plugin answering with a higher version. That rejection never reaches the
// plugin — plugin.register still returns OK — so a hardcoded version shows up only
// as registered=false in the management API. Checking an older host version here is
// the difference between catching that and shipping it.
var hostSchemaVersions = []uint32{5, schemaVersion}

func checkRegistration(plugin *C.cliproxy_plugin_api, configPath string) error {
	for _, hostSchema := range hostSchemaVersions {
		registration, errRegister := registerOnce(plugin, configPath, hostSchema)
		if errRegister != nil {
			return fmt.Errorf("host schema_version %d: %w", hostSchema, errRegister)
		}
		if registration.SchemaVersion != hostSchema {
			return fmt.Errorf("host announced schema_version %d but the plugin answered %d; the host would refuse to register it and leave registered=false", hostSchema, registration.SchemaVersion)
		}
	}
	fmt.Printf("step 2/5  plugin.register echoes the host schema_version and satisfies validPlugin()\n")
	return nil
}

// registerOnce performs one plugin.register call and checks everything the host
// would check before accepting the plugin.
func registerOnce(plugin *C.cliproxy_plugin_api, configPath string, hostSchema uint32) (registrationShape, error) {
	pluginConfig := fmt.Sprintf(
		"usage_url: %q\nconfig_path: %q\npoll_interval: 1h\nrequest_timeout: 3s\nlog_level: debug\n",
		usageURL, configPath)

	payload, errMarshal := json.Marshal(map[string]any{
		"config_yaml":    []byte(pluginConfig),
		"schema_version": hostSchema,
	})
	if errMarshal != nil {
		return registrationShape{}, errMarshal
	}

	raw, errCall := callPlugin(plugin, "plugin.register", payload)
	if errCall != nil {
		return registrationShape{}, errCall
	}
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return registrationShape{}, fmt.Errorf("plugin.register envelope: %w (raw: %s)", errUnmarshal, raw)
	}
	if !envelope.OK {
		return registrationShape{}, fmt.Errorf("plugin.register failed: %+v", envelope.Error)
	}

	var registration registrationShape
	if errUnmarshal := json.Unmarshal(envelope.Result, &registration); errUnmarshal != nil {
		return registrationShape{}, fmt.Errorf("plugin.register result: %w (raw: %s)", errUnmarshal, envelope.Result)
	}

	// Mirror the host's validPlugin() preconditions. An empty field here is not a
	// cosmetic problem: the host accepts the register RPC and then silently refuses
	// to register the plugin, so the only symptom is registered=false in the
	// management API with nothing in the plugin's own logs.
	for _, field := range []struct{ name, value string }{
		{"metadata.Name", registration.Metadata.Name},
		{"metadata.Version", registration.Metadata.Version},
		{"metadata.Author", registration.Metadata.Author},
		{"metadata.GitHubRepository", registration.Metadata.GitHubRepository},
	} {
		if strings.TrimSpace(field.value) == "" {
			return registrationShape{}, fmt.Errorf("%s is empty; the host's validPlugin() would reject the plugin and leave registered=false", field.name)
		}
	}

	caps := registration.Capabilities
	if !caps.Scheduler || !caps.UsagePlugin || !caps.ManagementAPI || !caps.QuotaProvider {
		return registrationShape{}, fmt.Errorf("registration capabilities did not decode: %+v", caps)
	}
	return registration, nil
}

func checkQuota(plugin *C.cliproxy_plugin_api, authID string) error {
	payload, errMarshal := json.Marshal(map[string]any{
		"auth_id":  authID,
		"provider": providerKey,
		"attributes": map[string]string{
			"api_key":  apiKey,
			"base_url": providerBase,
		},
	})
	if errMarshal != nil {
		return errMarshal
	}

	raw, errCall := callPlugin(plugin, "quota.fetch", payload)
	if errCall != nil {
		return errCall
	}
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return fmt.Errorf("quota.fetch envelope: %w (raw: %s)", errUnmarshal, raw)
	}
	if !envelope.OK {
		return fmt.Errorf("quota.fetch failed: %s (the usage fetch through host.http.do probably did not land)", envelope.Error.Message)
	}

	var response struct {
		Groups []struct {
			Buckets []struct {
				Window            string  `json:"window"`
				RemainingFraction float64 `json:"remainingFraction"`
			} `json:"buckets"`
		} `json:"groups"`
	}
	if errUnmarshal := json.Unmarshal(envelope.Result, &response); errUnmarshal != nil {
		return fmt.Errorf("quota.fetch result: %w (raw: %s)", errUnmarshal, envelope.Result)
	}
	if len(response.Groups) == 0 || len(response.Groups[0].Buckets) == 0 {
		return fmt.Errorf("quota.fetch returned no buckets: %s", envelope.Result)
	}
	bucket := response.Groups[0].Buckets[0]
	if bucket.Window != "rolling" {
		return fmt.Errorf("first bucket window = %q, want rolling", bucket.Window)
	}
	expectedRemaining := (100 - usagePercent) / 100
	if diff := bucket.RemainingFraction - expectedRemaining; diff > 0.001 || diff < -0.001 {
		return fmt.Errorf("remaining fraction = %v, want %v (host.http.do payload did not round-trip)", bucket.RemainingFraction, expectedRemaining)
	}
	fmt.Printf("step 3/5  quota.fetch served cached usage fetched through host.http.do (rolling %.0f%%)\n", usagePercent)
	return nil
}

func checkPick(plugin *C.cliproxy_plugin_api, authID string) error {
	payload, errMarshal := json.Marshal(map[string]any{
		"Provider": providerKey,
		"Candidates": []map[string]any{{
			"ID":       authID,
			"Provider": providerKey,
			"Attributes": map[string]string{
				"base_url": providerBase,
				"source":   fmt.Sprintf("config:%s[%s]", providerName, authID[len(authID)-12:]),
			},
		}},
		"Options": map[string]any{
			"Headers": map[string][]string{sessionHeader: {"abicheck-session"}},
		},
	})
	if errMarshal != nil {
		return errMarshal
	}

	raw, errCall := callPlugin(plugin, "scheduler.pick", payload)
	if errCall != nil {
		return errCall
	}
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return fmt.Errorf("scheduler.pick envelope: %w (raw: %s)", errUnmarshal, raw)
	}
	if !envelope.OK {
		return fmt.Errorf("scheduler.pick failed: %s", envelope.Error.Message)
	}

	// The response fields carry Go names, matching the host's marshalling of
	// pluginapi.SchedulerPickResponse.
	var response struct {
		AuthID  string `json:"AuthID"`
		Handled bool   `json:"Handled"`
	}
	if errUnmarshal := json.Unmarshal(envelope.Result, &response); errUnmarshal != nil {
		return fmt.Errorf("scheduler.pick result: %w (raw: %s)", errUnmarshal, envelope.Result)
	}
	if !response.Handled {
		return fmt.Errorf("scheduler.pick declined a request that should have been routed; the token recipe or config sync is off")
	}
	if response.AuthID != authID {
		return fmt.Errorf("scheduler.pick chose %q, want %q", response.AuthID, authID)
	}
	fmt.Println("step 4/5  scheduler.pick routed the session to the only credential")
	return nil
}

func callPlugin(plugin *C.cliproxy_plugin_api, method string, payload []byte) ([]byte, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var requestPtr *C.uint8_t
	if len(payload) > 0 {
		requestPtr = (*C.uint8_t)(C.CBytes(payload))
		defer C.free(unsafe.Pointer(requestPtr))
	}

	var response C.cliproxy_buffer
	rc := C.abicheckCallPlugin(plugin.call, cMethod, requestPtr, C.size_t(len(payload)), &response)
	if response.ptr != nil {
		defer C.abicheckFreePlugin(plugin.free_buffer, response.ptr, response.len)
	}
	if response.ptr == nil || response.len == 0 {
		// The plugin reports a failed method through the envelope rather than the
		// return code, so an empty buffer is the only thing to reject here; the
		// callers decode the envelope and surface its error.
		return nil, fmt.Errorf("%s returned an empty buffer (rc=%d)", method, int(rc))
	}
	return C.GoBytes(response.ptr, C.int(response.len)), nil
}

// abicheckHostCall answers the plugin's plugin→host calls. The real host routes
// these through the same JSON envelope, so returning a fixed usage payload is
// enough to prove the round trip.
//
//export abicheckHostCall
func abicheckHostCall(_ unsafe.Pointer, method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	name := C.GoString(method)
	var payload []byte
	if request != nil && requestLen > 0 {
		payload = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}

	var result any
	switch name {
	case "host.log":
		var entry struct {
			Level   string `json:"level"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(payload, &entry)
		if entry.Level == "error" || entry.Level == "warn" {
			fmt.Fprintf(os.Stderr, "           host.log[%s] %s\n", entry.Level, entry.Message)
		}
		result = map[string]any{}

	case "host.http.do":
		body := fmt.Sprintf(
			`{"usage":{"rolling":{"status":"ok","percent":%v,"resetsAt":"2026-09-23T14:39:09.347Z"},`+
				`"weekly":{"status":"ok","percent":1},"monthly":{"status":"ok","percent":1}}}`,
			usagePercent)
		result = map[string]any{
			"StatusCode": 200,
			"Headers":    map[string][]string{"Content-Type": {"application/json"}},
			"Body":       []byte(body),
		}

	default:
		raw, _ := json.Marshal(map[string]any{
			"ok":    false,
			"error": map[string]any{"code": "unsupported", "message": name},
		})
		writeHostBuffer(response, raw)
		return 0
	}

	raw, errMarshal := json.Marshal(map[string]any{"ok": true, "result": result})
	if errMarshal != nil {
		return 1
	}
	writeHostBuffer(response, raw)
	return 0
}

//export abicheckHostFree
func abicheckHostFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

func writeHostBuffer(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
