package soda

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/i2y/ramune"
	"github.com/i2y/ramune/workers"
	"github.com/pocketbase/pocketbase/core"
)

// requestSignals lets JS release the HTTP handler early via
// __detachResponse(reqID) while the executor VM continues awaiting
// ctx.waitUntil promises in the background. See wrapWorkersFetchHandler.
var (
	requestSignals sync.Map // int64 → chan struct{}
	nextRequestID  atomic.Int64
)

// workersCtxJS is the JS that declares __pending and the ctx argument
// passed to Workers-style fetch and scheduled handlers. Both handler
// kinds use the same ctx shape.
const workersCtxJS = `var __pending = [];
var __ctx = {
	waitUntil: function(p) {
		if (p && typeof p.then === "function") __pending.push(p);
	},
	passThroughOnException: function() {}
};`

// isWorkersStyle delegates to ramune/workers so the detection logic
// stays in sync with the generic Workers runtime.
func isWorkersStyle(code string) bool {
	return workers.IsWorkersStyle(code)
}

// transpileWorkersModule delegates to ramune/workers for the esbuild
// IIFE transform; Soda's executor pool continues to manage dispatch.
func transpileWorkersModule(filename string, code string) (string, error) {
	return workers.TranspileModule(filename, code)
}

// registerWorkersHandler inspects the default export captured by a
// previously executed IIFE and registers fetch/scheduled handlers.
//
// moduleCode is the full IIFE-transformed source.  Instead of
// serialising individual handler functions (which loses closure
// scope / imports), the entire module is re-evaluated in each
// executor VM so that closures, npm imports, and helper functions
// remain available.
//
// Must be called after loader.Exec(moduleCode) has run.
func registerWorkersHandler(
	app core.App,
	loader *ramune.Runtime,
	executors *vmsPool,
	proxy *typeProxy,
	filename string,
	moduleCode string,
	waitUntilTimeout time.Duration,
) error {
	mod, err := workers.ExtractModuleConfig(loader)
	if err != nil {
		return fmt.Errorf("[workers] %s: %w", filename, err)
	}

	if !mod.HasFetch && !mod.HasScheduled {
		app.Logger().Warn(
			"[workers] default export has neither fetch nor scheduled",
			slog.String("file", filename),
		)
		cleanupWorkersGlobal(loader)
		return nil
	}

	cleanupWorkersGlobal(loader)

	route := mod.Route
	cronExpr := mod.Cron
	hasFetch := mod.HasFetch
	hasScheduled := mod.HasScheduled

	// Register fetch handler.
	if hasFetch {
		if route == "" {
			route = "/{path...}"
		}
		cacheKey := "__wk_" + strings.ReplaceAll(filename, ".", "_")
		handler := wrapWorkersFetchHandler(executors, proxy, moduleCode, cacheKey, waitUntilTimeout)
		r := route
		app.OnServe().BindFunc(func(e *core.ServeEvent) error {
			for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
				e.Router.Route(method, r, handler)
			}
			return e.Next()
		})
		app.Logger().Info(
			"[workers] registered fetch handler",
			slog.String("file", filename),
			slog.String("route", route),
		)
	}

	// Register scheduled handler (uses the same per-VM caching as fetch).
	if hasScheduled {
		if cronExpr == "" {
			app.Logger().Warn(
				"[workers] scheduled handler has no cron expression, skipping",
				slog.String("file", filename),
			)
		} else {
			jobId := "workers:" + filename
			cronCacheKey := "__wk_cron_" + strings.ReplaceAll(filename, ".", "_")
			cronInitJS := buildModuleCacheJS(moduleCode, cronCacheKey)
			ce := cronExpr // capture
			err := app.Cron().Add(jobId, cronExpr, func() {
				err := executors.run(func(executor *ramune.Runtime) error {
					code := fmt.Sprintf(
						`(async function() {
							%s
							var __env = __buildEnv();
							var __event = { scheduledTime: Date.now(), cron: %q };
							%s
							await globalThis[%q].scheduled(__event, __env, __ctx);
							if (__pending.length) await Promise.allSettled(__pending);
						})()`,
						cronInitJS, ce, workersCtxJS, cronCacheKey,
					)
					_, err := executor.EvalAsync(code)
					return err
				})
				if err != nil {
					app.Logger().Error(
						"[workers] failed to execute scheduled handler",
						slog.String("file", filename),
						slog.String("jobId", jobId),
						slog.String("error", err.Error()),
					)
				}
			})
			if err != nil {
				return fmt.Errorf("[workers] %s: failed to register cron job: %w", filename, err)
			}
			app.Logger().Info(
				"[workers] registered scheduled handler",
				slog.String("file", filename),
				slog.String("cron", cronExpr),
			)
		}
	}

	return nil
}

// cleanupWorkersGlobal removes the temporary __workers_export from
// the loader VM after config has been extracted.  The executor VMs
// manage their own copies via buildModuleCacheJS.
func cleanupWorkersGlobal(loader *ramune.Runtime) {
	loader.Exec("delete globalThis.__workers_export;")
}

// buildModuleCacheJS returns a JS snippet that evaluates moduleCode
// once per executor VM and caches the default export under cacheKey.
// Subsequent calls in the same VM are a no-op (global already set).
func buildModuleCacheJS(moduleCode, cacheKey string) string {
	return fmt.Sprintf(
		`if (!globalThis[%q]) { %s globalThis[%q] = __workers_export.default; delete globalThis.__workers_export; }`,
		cacheKey, moduleCode, cacheKey,
	)
}

// wrapWorkersFetchHandler creates a Go handler function that bridges
// PocketBase's RequestEvent to a Workers-style fetch(Request, env, ctx)->Response.
//
// The module is evaluated once per executor VM and cached; subsequent
// requests only pay for the Request/Response plumbing.
//
// waitUntilTimeout bounds how long the executor VM is held after the
// response has been written, waiting for ctx.waitUntil() promises to
// settle. A value of 0 disables the timeout (wait indefinitely).
func wrapWorkersFetchHandler(executors *vmsPool, proxy *typeProxy, moduleCode string, cacheKey string, waitUntilTimeout time.Duration) func(*core.RequestEvent) error {
	// Pre-build the module-caching JS once (contains the full IIFE).
	// Only the per-request handles/method/URL are Sprintf'd per call.
	initJS := buildModuleCacheJS(moduleCode, cacheKey)

	// Non-positive → no timeout. Register defaults 0 to 30s; guard here
	// too for direct callers.
	waitUntilMs := int64(0)
	if waitUntilTimeout > 0 {
		waitUntilMs = waitUntilTimeout.Milliseconds()
	}

	return func(e *core.RequestEvent) error {
		reqID := nextRequestID.Add(1)
		signalCh := make(chan struct{})
		requestSignals.Store(reqID, signalCh)
		defer requestSignals.Delete(reqID)

		errCh := make(chan error, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					errCh <- fmt.Errorf("workers fetch handler panic: %v", r)
				}
			}()
			errCh <- executors.run(func(executor *ramune.Runtime) error {
				eventHandle := proxy.registry.register(e)
				defer proxy.registry.unregister(eventHandle)

				reqHandle := proxy.registry.register(e.Request)
				defer proxy.registry.unregister(reqHandle)

				code := fmt.Sprintf(
					`(async function() {
						%s
						var __wk = globalThis[%q];
						var __h = %d;
						var __rid = %d;
						var __e = __proxy_wrap("RequestEvent", __h);
						var __headers = __getGoRequestHeaders(%d);
						var __method = %q;
						var __body = (__method !== "GET" && __method !== "HEAD")
							? __readGoRequestBody(%d) : undefined;
						var __request = new Request(%q, {
							method: __method, headers: __headers, body: __body,
						});
						var __env = __buildEnv();
						%s
						try {
							var __response = await __wk.fetch(__request, __env, __ctx);
							if (!__response || typeof __response !== "object") {
								__writeWorkerResponse(__h, 204, "{}", "");
							} else {
								var __rh = {};
								if (__response.headers && typeof __response.headers.forEach === "function") {
									__response.headers.forEach(function(v, k) { __rh[k] = v; });
								}
								var __respBody = __response.body;
								if (__respBody && typeof __respBody.getReader === "function") {
									__writeWorkerResponseStart(__h, __response.status || 200, JSON.stringify(__rh));
									var __reader = __respBody.getReader();
									var __dec = new TextDecoder();
									while (true) {
										var __read = await __reader.read();
										if (__read.done) break;
										var __text = typeof __read.value === "string"
											? __read.value
											: __dec.decode(__read.value, {stream: true});
										if (__text) __writeWorkerResponseChunk(__h, __text);
									}
								} else {
									var __rb = "";
									if (typeof __response.text === "function") {
										__rb = await __response.text();
									} else if (__respBody != null) {
										__rb = String(__respBody);
									}
									__writeWorkerResponse(__h, __response.status || 200, JSON.stringify(__rh), __rb);
								}
							}
						} finally {
							// Release the HTTP handler even if fetch threw so the
							// client sees an error response rather than a hang.
							__detachResponse(__rid);
						}
						if (__pending.length) {
							var __wait = Promise.allSettled(__pending);
							var __timeoutMs = %d;
							if (__timeoutMs > 0) {
								var __timeoutP = new Promise(function(resolve){ setTimeout(resolve, __timeoutMs); });
								await Promise.race([__wait, __timeoutP]);
							} else {
								await __wait;
							}
						}
					})()`,
					initJS,
					cacheKey,
					eventHandle,
					reqID,
					reqHandle,
					e.Request.Method,
					reqHandle,
					fullRequestURL(e.Request),
					workersCtxJS,
					waitUntilMs,
				)

				_, err := executor.EvalAsync(code)
				return err
			})
		}()

		select {
		case <-signalCh:
			return nil
		case err := <-errCh:
			return err
		}
	}
}

// workersRequestBinds registers Go helper functions on a VM that the
// Workers fetch handler wrapper uses to read from Go's *http.Request
// and write to Go's http.ResponseWriter.
func workersRequestBinds(vm *ramune.Runtime, proxy *typeProxy) {
	vm.RegisterFunc("__readGoRequestBody", func(args []any) (any, error) {
		if len(args) < 1 {
			return "", nil
		}
		handle, ok := toInt64(args[0])
		if !ok {
			return "", fmt.Errorf("__readGoRequestBody: invalid handle")
		}
		obj, found := proxy.registry.get(handle)
		if !found {
			return "", fmt.Errorf("__readGoRequestBody: handle not found")
		}
		req, ok := obj.(*http.Request)
		if !ok {
			return "", fmt.Errorf("__readGoRequestBody: not an *http.Request")
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return "", err
		}
		return string(body), nil
	})

	vm.RegisterFunc("__getGoRequestHeaders", func(args []any) (any, error) {
		if len(args) < 1 {
			return map[string]any{}, nil
		}
		handle, ok := toInt64(args[0])
		if !ok {
			return map[string]any{}, nil
		}
		obj, found := proxy.registry.get(handle)
		if !found {
			return map[string]any{}, nil
		}
		req, ok := obj.(*http.Request)
		if !ok {
			return map[string]any{}, nil
		}
		headers := make(map[string]any, len(req.Header))
		for k, v := range req.Header {
			if len(v) == 1 {
				headers[k] = v[0]
			} else {
				headers[k] = strings.Join(v, ", ")
			}
		}
		return headers, nil
	})

	vm.RegisterFunc("__detachResponse", func(args []any) (any, error) {
		if len(args) < 1 {
			return nil, nil
		}
		reqID, ok := toInt64(args[0])
		if !ok {
			return nil, nil
		}
		if chAny, loaded := requestSignals.LoadAndDelete(reqID); loaded {
			ch, ok := chAny.(chan struct{})
			if ok {
				close(ch)
			}
		}
		return nil, nil
	})

	vm.RegisterFunc("__writeWorkerResponseStart", func(args []any) (any, error) {
		if len(args) < 3 {
			return nil, fmt.Errorf("__writeWorkerResponseStart requires 3 args")
		}
		event, err := eventFromHandle(proxy, args[0], "__writeWorkerResponseStart")
		if err != nil {
			return nil, err
		}
		status := 200
		if s, ok := toInt64(args[1]); ok {
			status = int(s)
		}
		headersJSON, _ := args[2].(string)
		applyWorkerResponseHeaders(event, headersJSON)
		event.Response.WriteHeader(status)
		_ = event.Flush()
		return nil, nil
	})

	vm.RegisterFunc("__writeWorkerResponseChunk", func(args []any) (any, error) {
		if len(args) < 2 {
			return nil, nil
		}
		event, err := eventFromHandle(proxy, args[0], "__writeWorkerResponseChunk")
		if err != nil {
			return nil, err
		}
		text, _ := args[1].(string)
		if text == "" {
			return nil, nil
		}
		if _, err := io.WriteString(event.Response, text); err != nil {
			return nil, err
		}
		_ = event.Flush()
		return nil, nil
	})

	vm.RegisterFunc("__writeWorkerResponse", func(args []any) (any, error) {
		if len(args) < 4 {
			return nil, fmt.Errorf("__writeWorkerResponse requires 4 args")
		}
		event, err := eventFromHandle(proxy, args[0], "__writeWorkerResponse")
		if err != nil {
			return nil, err
		}

		status := 200
		if s, ok := toInt64(args[1]); ok {
			status = int(s)
		}

		headersJSON, _ := args[2].(string)
		body, _ := args[3].(string)

		applyWorkerResponseHeaders(event, headersJSON)
		event.Response.WriteHeader(status)
		if body != "" {
			event.Response.Write([]byte(body))
		}

		return nil, nil
	})
}

// eventFromHandle resolves a proxy handle to the underlying RequestEvent.
// Used by the __writeWorkerResponse* bindings.
func eventFromHandle(proxy *typeProxy, arg any, fn string) (*core.RequestEvent, error) {
	handle, ok := toInt64(arg)
	if !ok {
		return nil, fmt.Errorf("%s: invalid event handle", fn)
	}
	obj, found := proxy.registry.get(handle)
	if !found {
		return nil, fmt.Errorf("%s: event handle not found", fn)
	}
	event, ok := obj.(*core.RequestEvent)
	if !ok {
		return nil, fmt.Errorf("%s: not a *RequestEvent", fn)
	}
	return event, nil
}

// applyWorkerResponseHeaders copies JSON-encoded headers onto the event.
// Empty or "{}" input is a no-op. Malformed JSON is silently ignored so
// a broken user-supplied object does not prevent a status+body write.
func applyWorkerResponseHeaders(event *core.RequestEvent, headersJSON string) {
	if headersJSON == "" || headersJSON == "{}" {
		return
	}
	var headers map[string]string
	if err := json.Unmarshal([]byte(headersJSON), &headers); err != nil {
		return
	}
	for k, v := range headers {
		event.Response.Header().Set(k, v)
	}
}

// fullRequestURL reconstructs the full URL from an *http.Request.
func fullRequestURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	return scheme + "://" + host + r.RequestURI
}

// toInt64 converts a JS number (which arrives as float64) to int64.
func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	}
	return 0, false
}

// serializeFuncArgsJS is executed on the loader VM right after the Go
// binding functions (routerAdd, cronAdd, on*) are registered.
//
// Ramune >= v0.4 passes JS functions to Go callbacks as *ramune.JSFunc
// instead of auto-serialising to strings.  The executor-pool model
// requires string-form callbacks so they can be re-evaluated in any VM.
// This wrapper converts function arguments to their source via
// .toString() before they cross the Go boundary.
const serializeFuncArgsJS = `
(function() {
	function wrapFn(orig) {
		return function() {
			var args = [];
			for (var i = 0; i < arguments.length; i++) {
				var a = arguments[i];
				if (typeof a === "function") {
					args.push(a.toString());
				} else if (a && typeof a === "object" && typeof a.serializedFunc === "function") {
					a = Object.assign({}, a);
					a.serializedFunc = a.serializedFunc.toString();
					args.push(a);
				} else {
					args.push(a);
				}
			}
			return orig.apply(null, args);
		};
	}

	// Wrap routerAdd, routerUse, cronAdd
	if (typeof routerAdd === "function")  globalThis.routerAdd  = wrapFn(routerAdd);
	if (typeof routerUse === "function")  globalThis.routerUse  = wrapFn(routerUse);
	if (typeof cronAdd === "function")    globalThis.cronAdd    = wrapFn(cronAdd);

	// Wrap all on* hook functions (onRecordCreate, onRecordUpdate, etc.)
	Object.getOwnPropertyNames(globalThis).forEach(function(key) {
		if (/^on[A-Z]/.test(key) && typeof globalThis[key] === "function") {
			globalThis[key] = wrapFn(globalThis[key]);
		}
	});
})();
`
