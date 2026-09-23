// Declarations shared by the abicheck program and its C helpers.
//
// This header holds only types and declarations: it is included both from the
// cgo preamble (which is compiled twice) and from bridge.c (which is compiled
// once and owns every definition).

#ifndef ABICHECK_BRIDGE_H
#define ABICHECK_BRIDGE_H

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

typedef int (*cliproxy_plugin_init_fn)(const cliproxy_host_api*, cliproxy_plugin_api*);

void* abicheckOpen(const char* path);
void* abicheckSymbol(void* handle, const char* name);
const char* abicheckError(void);
int abicheckClose(void* handle);
int abicheckInit(void* fn, const cliproxy_host_api* host, cliproxy_plugin_api* plugin);
int abicheckCallPlugin(cliproxy_plugin_call_fn fn, char* method, uint8_t* request, size_t requestLen, cliproxy_buffer* response);
void abicheckFreePlugin(cliproxy_plugin_free_fn fn, void* ptr, size_t len);
void abicheckShutdownPlugin(cliproxy_plugin_shutdown_fn fn);

// Adapters with the host's const-qualified signature. They are referenced by
// address from Go to fill cliproxy_host_api, so they need external linkage.
int abicheckHostCallBridge(void* ctx, const char* method, const uint8_t* request, size_t requestLen, cliproxy_buffer* response);
void abicheckHostFreeBridge(void* ptr, size_t len);

#endif
