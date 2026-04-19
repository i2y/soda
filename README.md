# Soda

A drop-in Ramune-backed replacement for PocketBase's JSVM plugin.

Soda is a Go library that layers [Ramune](https://github.com/i2y/ramune) —
a modern JS/TS runtime with JIT, async/await, built-in TypeScript, and
npm support — on top of upstream [PocketBase](https://github.com/pocketbase/pocketbase),
as a plug-in alternative to `github.com/pocketbase/pocketbase/plugins/jsvm`.

## Why

PocketBase's built-in JSVM uses [goja](https://github.com/dop251/goja), an
ES5-ish pure-Go interpreter. That keeps the PB binary simple, but it means
`pb_hooks/*.pb.js` cannot:

- `await` or use Promises
- call modern `fetch()` (you get `$http.send()` instead)
- import npm packages
- run TypeScript directly

Soda registers a parallel JSVM plugin that does all of the above, without
forking PocketBase. Your `main.go` stays upstream; you just swap the
plugin import.

| Feature | upstream `plugins/jsvm` (goja) | `soda` (Ramune) |
|---|---|---|
| async/await, Promises | No | Yes |
| `fetch()` (Web standard) | No | Yes |
| TypeScript (`.pb.ts`) direct execution | No | Yes |
| npm packages | No | Yes |
| Web Crypto, Web Streams | No | Yes |
| Workers-style `export default { fetch, scheduled }` | No | Yes |
| `ctx.waitUntil` post-response background work | No | Yes |
| Performance | Interpreter | QuickJS-NG on wazero (default) or JSC+JIT |

## Quick start

```go
// main.go
package main

import (
    "log"

    "github.com/pocketbase/pocketbase"

    "github.com/i2y/soda"
)

func main() {
    app := pocketbase.New()
    soda.MustRegister(app, soda.Config{})
    if err := app.Start(); err != nil {
        log.Fatal(err)
    }
}
```

Build and run:

```bash
# qjswasm backend (recommended, pure Go, cross-platform single binary)
go build -tags qjswasm -o myapp . && ./myapp serve

# or JSC backend (JIT on macOS/Linux; requires libjavascriptcoregtk-4.1 on Linux)
go build -o myapp . && ./myapp serve
```

### Try the packaged demo

`examples/basic/` ships a ready-to-run program that drives every major
Soda feature. Clone, build, and open the landing page:

```bash
cd examples/basic
go build -tags qjswasm -o demo .
./demo serve
# → http://127.0.0.1:8090/
```

The landing page is rendered by Hono (declared via `soda.toml`) and
links to four Workers-style endpoints, each in its own `pb_hooks/*.pb.ts`
file:

| Endpoint | Hook file | What it demonstrates |
|---|---|---|
| `GET /` and `/about` | `hono.pb.ts` | Hono framework via npm, `c.html()` + `Content-Type: text/html` |
| `GET /api/hello?name=X` | `hello.pb.ts` | Minimal `export default { fetch }` returning `Response.json(...)` |
| `GET /api/crypto` | `crypto.pb.ts` | `crypto.subtle.digest("SHA-256", ...)`, `crypto.randomUUID()`, `crypto.getRandomValues()` |
| `GET /api/sse` (use `curl -N`) | `sse.pb.ts` | Response body as a `ReadableStream`; chunks flushed at 400 ms intervals |
| `GET /api/waituntil?key=X` | `waituntil.pb.ts` | `ctx.waitUntil(promise)` — HTTP response in ~60 ms while a background task writes to `env.KV` 1.5 s later. Read back with `?mode=read`. |

One qjswasm binary, zero runtime dependencies, every feature in the
feature table above exercised end-to-end.

### Writing your own hooks

Drop classic-style hooks in `pb_hooks/` the same way you would for upstream
PocketBase — they just get async/TS/npm for free:

```typescript
// pb_hooks/notify.pb.ts
onRecordAfterCreateSuccess(async (e) => {
    await fetch("https://hooks.slack.com/services/...", {
        method: "POST",
        body: JSON.stringify({ text: `New user: ${e.record.get("name")}` }),
    });
    e.next();
}, "users");
```

Or write Workers-style `export default` modules in the same directory:

```typescript
// pb_hooks/api.pb.ts
export default {
    route: "/api/users/{id}",

    async fetch(request: Request, env: Env, ctx: ExecutionContext): Promise<Response> {
        const url = new URL(request.url);
        const user = env.DB.prepare("SELECT * FROM users WHERE id = ?")
            .bind(url.pathname.split("/").pop())
            .first();

        // background work, doesn't delay the response:
        ctx.waitUntil(env.KV.put("last_read:" + user?.id, new Date().toISOString()));

        return Response.json(user || { error: "not found" });
    },
} satisfies WorkersHandler;
```

## Declarative config (`soda.toml`)

Place a `soda.toml` next to `pb_hooks/` to declare npm dependencies, Ramune
sandbox permissions, and named KV bindings — no Go changes needed:

```toml
[dependencies]
hono = "*"

[permissions]
net = "granted"
read = "granted"
write = "denied"
run = "denied"
net_hosts = ["api.stripe.com"]

[[kv_namespaces]]
binding = "SESSIONS"
namespace = "sessions"
```

Go-side `soda.Config` values always win; TOML only fills unset fields.

## Build tags

Soda inherits Ramune's three JS backends:

| Tag | Engine | Platforms | Runtime deps | Notes |
|-----|--------|-----------|--------------|-------|
| `qjswasm` (recommended) | QuickJS-NG on wazero | macOS, Linux, Windows | None | Pure Go, single binary, `FROM scratch` Docker; no JS JIT |
| _(no tag)_ | JavaScriptCore via purego | macOS, Linux | macOS: built-in; Linux: `libjavascriptcoregtk-4.1` | JIT, fastest on CPU-bound JS |
| `goja` | goja (upstream) | All | None | Legacy fallback |

## Relationship to upstream PocketBase

Soda is **not a fork**. It imports `github.com/pocketbase/pocketbase` as a
library and registers a parallel JSVM plugin through PocketBase's public
plugin API. You can swap back to upstream `plugins/jsvm` by changing the
import; nothing about your `pb_hooks/` filenames or collection schemas is
Soda-specific beyond the features Soda adds (TypeScript, Workers-style
modules, etc.).

Everything else PocketBase ships — the admin dashboard at `/_/`, the
collections REST API, auth, realtime subscriptions, file handling,
migrations — keeps working exactly as upstream, because Soda only
replaces the JSVM plugin.

## About the name

[Ramune](https://en.wikipedia.org/wiki/Ramune) is a Japanese soft drink —
a soda. The Go plugin that exposes the Ramune runtime to PocketBase is
named Soda, i.e. the same drink in English. Pair them and the host
program reads as `soda.MustRegister(app, soda.Config{})` — soda on top
of PocketBase, powered by Ramune underneath.

## Credits

- [PocketBase](https://github.com/pocketbase/pocketbase) by Gani Georgiev — MIT License. Soda is a drop-in plugin layered on top; it depends on upstream PocketBase and does not modify it.
- [Ramune](https://github.com/i2y/ramune) — the Go-embeddable JS/TS runtime Soda delegates to.

## License

MIT — see [LICENSE.md](LICENSE.md).
