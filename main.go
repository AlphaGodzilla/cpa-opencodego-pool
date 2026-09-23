// Command opencodego-pool builds the CLIProxyAPI plugin shared object.
//
// The binary exports the C ABI the host expects and nothing else: every method
// call arrives as (method name, JSON payload) and leaves as a JSON envelope.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"sync"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"

	"opencodego-pool/internal/hostbridge"
	"opencodego-pool/internal/service"
)

// abiVersion tracks the native C ABI shape, not the RPC contract.
const abiVersion uint32 = 1

var (
	serviceOnce sync.Once
	shared      *service.Service
)

// instance lazily builds the service and wires the host bridge into it.
func instance() *service.Service {
	serviceOnce.Do(func() {
		hostbridge.Install(callHostAPI)
		shared = service.New(service.DefaultPluginID)
	})
	return shared
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(abiVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}

	var payload []byte
	if request != nil && requestLen > 0 {
		payload = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}

	raw, errHandle := dispatch(C.GoString(method), payload)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	if shared != nil {
		shared.Shutdown()
	}
}

// dispatch routes one host method call. Unknown methods are reported rather than
// silently accepted, so a host/plugin version mismatch shows up in the log.
func dispatch(method string, payload []byte) ([]byte, error) {
	svc := instance()

	switch method {
	case pluginabi.MethodPluginRegister:
		result, errRegister := svc.Register(payload)
		if errRegister != nil {
			return nil, errRegister
		}
		return okEnvelope(result)

	case pluginabi.MethodPluginReconfigure:
		result, errReconfigure := svc.Reconfigure(payload)
		if errReconfigure != nil {
			return nil, errReconfigure
		}
		return okEnvelope(result)

	case pluginabi.MethodPluginQuiesce:
		svc.Quiesce()
		return okEnvelope(struct{}{})

	case pluginabi.MethodSchedulerPick:
		result, errPick := svc.Pick(payload)
		if errPick != nil {
			return nil, errPick
		}
		return okEnvelope(result)

	case pluginabi.MethodUsageHandle:
		if errUsage := svc.HandleUsage(payload); errUsage != nil {
			return nil, errUsage
		}
		return okEnvelope(struct{}{})

	case pluginabi.MethodQuotaIdentifier:
		return okEnvelope(svc.Identify())

	case pluginabi.MethodQuotaDescribe:
		return okEnvelope(svc.DescribeQuota())

	case pluginabi.MethodQuotaFetch:
		result, errFetch := svc.FetchQuota(payload)
		if errFetch != nil {
			return nil, errFetch
		}
		return okEnvelope(result)

	case pluginabi.MethodQuotaReset:
		return okEnvelope(svc.ResetQuota())

	case pluginabi.MethodManagementRegister:
		return okEnvelope(svc.RegisterManagement())

	case pluginabi.MethodManagementHandle:
		result, errManaged := svc.HandleManagement(payload)
		if errManaged != nil {
			return nil, errManaged
		}
		return okEnvelope(result)

	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// callHostAPI performs one plugin→host RPC through the stored host function table.
func callHostAPI(method string, payload []byte) ([]byte, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var requestPtr *C.uint8_t
	if len(payload) > 0 {
		requestPtr = (*C.uint8_t)(C.CBytes(payload))
		defer C.free(unsafe.Pointer(requestPtr))
	}

	var response C.cliproxy_buffer
	rc := C.call_host_api(cMethod, requestPtr, C.size_t(len(payload)), &response)
	if response.ptr != nil {
		defer C.free_host_buffer(response.ptr, response.len)
	}
	if rc != 0 {
		return nil, fmt.Errorf("host call %s failed with rc=%d", method, rc)
	}
	if response.ptr == nil || response.len == 0 {
		return []byte(`{"ok":true}`), nil
	}
	return C.GoBytes(response.ptr, C.int(response.len)), nil
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func okEnvelope(value any) ([]byte, error) {
	result, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: result})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

// writeResponse hands a byte buffer to the host. The host frees it through the
// plugin's free_buffer export, so it must come from C.CBytes.
func writeResponse(response *C.cliproxy_buffer, raw []byte) {
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
