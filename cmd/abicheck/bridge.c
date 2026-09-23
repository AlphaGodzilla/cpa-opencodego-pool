// C side of the abicheck harness: the dynamic-loading helpers and the two
// adapters that give the host function table the const-qualified signature the
// real host uses.
//
// Listed as "C" via #cgo, and compiled exactly once.

#include "bridge.h"

#include <dlfcn.h>

#include "_cgo_export.h"

int abicheckHostCallBridge(void* ctx, const char* method, const uint8_t* request, size_t requestLen, cliproxy_buffer* response) {
	// _cgo_export.h declares these with Go's mapping, which drops const.
	return abicheckHostCall(ctx, (char*)method, (uint8_t*)request, requestLen, response);
}

void abicheckHostFreeBridge(void* ptr, size_t len) {
	abicheckHostFree(ptr, len);
}

void* abicheckOpen(const char* path) {
	return dlopen(path, RTLD_NOW | RTLD_LOCAL);
}

void* abicheckSymbol(void* handle, const char* name) {
	return dlsym(handle, name);
}

const char* abicheckError(void) {
	return dlerror();
}

int abicheckClose(void* handle) {
	return dlclose(handle);
}

int abicheckInit(void* fn, const cliproxy_host_api* host, cliproxy_plugin_api* plugin) {
	return ((cliproxy_plugin_init_fn)fn)(host, plugin);
}

int abicheckCallPlugin(cliproxy_plugin_call_fn fn, char* method, uint8_t* request, size_t requestLen, cliproxy_buffer* response) {
	return fn(method, request, requestLen, response);
}

void abicheckFreePlugin(cliproxy_plugin_free_fn fn, void* ptr, size_t len) {
	fn(ptr, len);
}

void abicheckShutdownPlugin(cliproxy_plugin_shutdown_fn fn) {
	fn();
}
