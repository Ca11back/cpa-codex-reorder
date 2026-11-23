#include "abi.h"
#include "_cgo_export.h"

static const cliproxy_host_api *stored_host;

#if defined(__APPLE__) && defined(__x86_64__)
#include <pthread.h>

// On Intel macOS, both Go runtimes use the same thread-local slot for the
// current goroutine. Cross the ABI on a fresh native thread so the receiving
// runtime does not find the other runtime's goroutine in that slot.
static int run_on_native_thread(void *(*run)(void *), void *request) {
    pthread_t thread;
    if (pthread_create(&thread, NULL, run, request) != 0) {
        return 1;
    }
    pthread_join(thread, NULL);
    return 0;
}

struct init_request {
    cliproxy_host_api *host;
    cliproxy_plugin_api *plugin;
    int result;
};

static void *run_init(void *arg) {
    struct init_request *request = arg;
    request->result = codexReorderInit(request->host, request->plugin);
    return NULL;
}

struct call_request {
    const char *method;
    const uint8_t *body;
    size_t len;
    cliproxy_buffer *response;
    int result;
};

static void *run_plugin_call(void *arg) {
    struct call_request *request = arg;
    request->result = cliproxyPluginCall((char *)request->method, (uint8_t *)request->body, request->len, request->response);
    return NULL;
}

static int isolated_plugin_call(char *method, uint8_t *body, size_t len, cliproxy_buffer *response) {
    struct call_request request = {method, body, len, response, 1};
    run_on_native_thread(run_plugin_call, &request);
    return request.result;
}

static void *run_shutdown(void *unused) {
    (void)unused;
    cliproxyPluginShutdown();
    return NULL;
}

static void isolated_shutdown(void) {
    run_on_native_thread(run_shutdown, NULL);
}

static void isolated_plugin_free(void *ptr, size_t len) {
    (void)len;
    free(ptr);
}

static void *run_host_call(void *arg) {
    struct call_request *request = arg;
    request->result = stored_host->call(stored_host->host_ctx, request->method, request->body, request->len, request->response);
    return NULL;
}

struct free_request {
    void *ptr;
    size_t len;
};

static void *run_host_free(void *arg) {
    struct free_request *request = arg;
    stored_host->free_buffer(request->ptr, request->len);
    return NULL;
}
#endif

#if defined(_WIN32)
__declspec(dllexport)
#endif
int cliproxy_plugin_init(cliproxy_host_api *host, cliproxy_plugin_api *plugin) {
#if defined(__APPLE__) && defined(__x86_64__)
    struct init_request request = {host, plugin, 1};
    run_on_native_thread(run_init, &request);
    return request.result;
#else
    return codexReorderInit(host, plugin);
#endif
}

void register_api(const cliproxy_host_api *host, cliproxy_plugin_api *plugin, uint32_t version) {
    stored_host = host;
    plugin->abi_version = version;
#if defined(__APPLE__) && defined(__x86_64__)
    plugin->call = isolated_plugin_call;
    plugin->free_buffer = isolated_plugin_free;
    plugin->shutdown = isolated_shutdown;
#else
    plugin->call = cliproxyPluginCall;
    plugin->free_buffer = cliproxyPluginFree;
    plugin->shutdown = cliproxyPluginShutdown;
#endif
}

int call_host_api(const char *method, const uint8_t *body, size_t len, cliproxy_buffer *response) {
    if (stored_host == NULL || stored_host->call == NULL) {
        return 1;
    }
#if defined(__APPLE__) && defined(__x86_64__)
    struct call_request request = {method, body, len, response, 1};
    run_on_native_thread(run_host_call, &request);
    return request.result;
#else
    return stored_host->call(stored_host->host_ctx, method, body, len, response);
#endif
}

void free_host_buffer(void *ptr, size_t len) {
    if (stored_host == NULL || stored_host->free_buffer == NULL || ptr == NULL) {
        return;
    }
#if defined(__APPLE__) && defined(__x86_64__)
    struct free_request request = {ptr, len};
    run_on_native_thread(run_host_free, &request);
#else
    stored_host->free_buffer(ptr, len);
#endif
}
