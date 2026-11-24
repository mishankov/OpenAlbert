#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#include "quickjs.h"

#define EXPORT(name) __attribute__((export_name(#name)))
#define MAX_RESULT_BYTES (256 * 1024)

__attribute__((import_module("openalbert"), import_name("enqueue_tool")))
extern int32_t enqueue_tool(const char *, size_t, const char *, size_t);

static JSRuntime *runtime;
static JSContext *context;
static JSValue bridge;
static const char *result;
static size_t result_size;

EXPORT(allocate) void *allocate(size_t size) {
    return malloc(size);
}

EXPORT(release) void release(void *buffer) {
    free(buffer);
}

static JSValue enqueue(JSContext *ctx, JSValueConst this_value,
                       int argc, JSValueConst *argv) {
    (void)this_value;
    if (argc != 2 || !JS_IsString(argv[0]) || !JS_IsString(argv[1])) {
        return JS_ThrowTypeError(ctx, "expected a tool name and JSON arguments");
    }
    size_t name_size, arguments_size;
    const char *name = JS_ToCStringLen(ctx, &name_size, argv[0]);
    if (!name) {
        return JS_EXCEPTION;
    }
    const char *arguments = JS_ToCStringLen(ctx, &arguments_size, argv[1]);
    if (!arguments) {
        JS_FreeCString(ctx, name);
        return JS_EXCEPTION;
    }
    int32_t id = enqueue_tool(name, name_size, arguments, arguments_size);
    JS_FreeCString(ctx, arguments);
    JS_FreeCString(ctx, name);
    if (id < 0) {
        return JS_ThrowRangeError(ctx, "tool call rejected");
    }
    return JS_NewInt32(ctx, id);
}

static void clear_result(void) {
    if (result) {
        JS_FreeCString(context, result);
        result = NULL;
        result_size = 0;
    }
}

static int save_result(JSValue value) {
    if (JS_IsException(value)) {
        value = JS_GetException(context);
        result = JS_ToCStringLen(context, &result_size, value);
        JS_FreeValue(context, value);
        return -1;
    }
    if (JS_IsString(value)) {
        result = JS_ToCStringLen(context, &result_size, value);
    }
    JS_FreeValue(context, value);
    return 0;
}

EXPORT(initialize) int initialize(const char *bootstrap, size_t size) {
    runtime = JS_NewRuntime();
    if (!runtime) {
        return -1;
    }
    JS_SetMemoryLimit(runtime, 24 * 1024 * 1024);
    JS_SetMaxStackSize(runtime, 512 * 1024);
    context = JS_NewContext(runtime);
    if (!context) {
        return -1;
    }
    JSValue global = JS_GetGlobalObject(context);
    JS_SetPropertyStr(context, global, "__openalbertEnqueue",
                      JS_NewCFunction(context, enqueue, "enqueueTool", 2));
    JS_FreeValue(context, global);
    bridge = JS_Eval(context, bootstrap, size, "bootstrap.js", JS_EVAL_TYPE_GLOBAL);
    if (JS_IsException(bridge)) {
        return save_result(bridge);
    }
    return 0;
}

EXPORT(invoke) int invoke(uint32_t operation, const char *data, size_t size) {
    static const char *const methods[] = {"start", "settle", "snapshot"};
    clear_result();
    if (operation >= sizeof(methods) / sizeof(methods[0])) {
        return -1;
    }
    JSValue method = JS_GetPropertyStr(context, bridge, methods[operation]);
    if (JS_IsException(method)) {
        return save_result(method);
    }
    JSValue arg = JS_NewStringLen(context, data, size);
    JSValue value = JS_IsException(arg) ? arg : JS_Call(context, method, bridge, 1, &arg);
    JS_FreeValue(context, arg);
    JS_FreeValue(context, method);
    if (JS_IsException(value)) {
        return save_result(value);
    }
    int jobs;
    JSContext *job_context;
    while ((jobs = JS_ExecutePendingJob(runtime, &job_context)) > 0) {
    }
    if (jobs < 0) {
        JS_FreeValue(context, value);
        return save_result(JS_EXCEPTION);
    }
    return save_result(value);
}

EXPORT(result_pointer) const char *result_pointer(void) {
    return result;
}

EXPORT(result_length) size_t result_length(void) {
    return result_size <= MAX_RESULT_BYTES ? result_size : 0;
}
