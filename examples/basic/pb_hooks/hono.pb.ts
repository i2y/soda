import { Hono } from "hono";

const app = new Hono();

const layout = (title: string, body: string) => `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>${title}</title>
  <style>
    * { margin: 0; padding: 0; box-sizing: border-box; }
    body { font-family: -apple-system, BlinkMacSystemFont, sans-serif; background: #0f172a; color: #e2e8f0; min-height: 100vh; display: flex; align-items: center; justify-content: center; }
    .container { max-width: 720px; padding: 2rem; text-align: center; }
    h1 { font-size: 2.5rem; background: linear-gradient(135deg, #38bdf8, #818cf8); -webkit-background-clip: text; -webkit-text-fill-color: transparent; margin-bottom: 1rem; }
    p { color: #94a3b8; line-height: 1.6; margin-bottom: 1.5rem; }
    a { color: #38bdf8; text-decoration: none; }
    a:hover { text-decoration: underline; }
    dl { display: grid; grid-template-columns: auto 1fr; gap: 0.75rem 1.5rem; text-align: left; max-width: 400px; margin: 0 auto; }
    dt { color: #64748b; font-size: 0.875rem; }
    dd { color: #e2e8f0; }
    .demos { display: grid; grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: 0.75rem; margin-top: 2rem; text-align: left; }
    .demo { background: #1e293b; border: 1px solid #334155; border-radius: 8px; padding: 1rem 1.25rem; }
    .demo h3 { color: #f1f5f9; font-size: 1rem; margin-bottom: 0.25rem; }
    .demo code { color: #38bdf8; font-size: 0.85rem; font-family: ui-monospace, monospace; }
    .demo p { color: #94a3b8; font-size: 0.85rem; margin: 0.35rem 0 0; line-height: 1.4; }
    .badge { display: inline-block; padding: 0.1rem 0.55rem; font-size: 0.7rem; border-radius: 999px; background: #0ea5e9; color: #0f172a; font-weight: 600; margin-left: 0.4rem; vertical-align: middle; }
  </style>
</head>
<body><div class="container">${body}</div></body>
</html>`;

app.get("/", (c) =>
  c.html(layout("Soda + Hono", `
    <h1>Soda + Hono</h1>
    <p>Upstream PocketBase as a library, Ramune JS engine via the Soda plugin,
       Hono routing the HTTP surface. No fork, no custom PB binary.</p>
    <p><a href="/about">About &rarr;</a></p>

    <div class="demos">
      <div class="demo">
        <h3>Workers JSON <span class="badge">fetch</span></h3>
        <a href="/api/hello?name=Soda"><code>GET /api/hello</code></a>
        <p>export default { fetch } returning Response.json().</p>
      </div>
      <div class="demo">
        <h3>Web Crypto <span class="badge">crypto.subtle</span></h3>
        <a href="/api/crypto"><code>GET /api/crypto</code></a>
        <p>crypto.subtle.digest, randomUUID, getRandomValues.</p>
      </div>
      <div class="demo">
        <h3>Streaming response <span class="badge">SSE</span></h3>
        <code>curl -N /api/sse</code>
        <p>ReadableStream body flushed chunk-by-chunk (5 ticks).</p>
      </div>
      <div class="demo">
        <h3>ctx.waitUntil <span class="badge">bg work</span></h3>
        <a href="/api/waituntil?key=demo"><code>GET /api/waituntil</code></a>
        <p>Response returns fast; bg task writes to env.KV after 1.5s.</p>
      </div>
    </div>
  `)),
);

app.get("/about", (c) =>
  c.html(layout("About - Soda + Hono", `
    <h1 style="font-size:2rem">About</h1>
    <dl>
      <dt>Runtime</dt>    <dd>Soda (Ramune)</dd>
      <dt>Framework</dt>  <dd>Hono (via soda.toml dependency)</dd>
      <dt>Backend</dt>    <dd>PocketBase (SQLite)</dd>
      <dt>Server time</dt><dd>${new Date().toISOString()}</dd>
    </dl>
    <p style="margin-top:2rem"><a href="/">&larr; Home</a></p>
  `)),
);

export default {
  async fetch(request: Request, _env: Env): Promise<Response> {
    return app.fetch(request);
  },
} satisfies WorkersHandler;
