// ctx.waitUntil demo — the fetch handler returns immediately while a
// background promise continues to run on the executor VM, just like
// Cloudflare Workers. Soda persists the eventual result to env.KV so
// you can read it back with the ?mode=read query.
//
//   # schedule the background write (returns in a couple of ms)
//   curl -w '\ntime=%{time_total}s\n' 'http://127.0.0.1:8094/api/waituntil?key=demo'
//
//   # immediately afterwards — the background task has not finished yet
//   curl 'http://127.0.0.1:8094/api/waituntil?key=demo&mode=read'
//
//   # after ~1.5 seconds the value will be there
//   sleep 2 && curl 'http://127.0.0.1:8094/api/waituntil?key=demo&mode=read'
export default {
  route: "/api/waituntil",

  async fetch(
    request: Request,
    env: Env,
    ctx: ExecutionContext,
  ): Promise<Response> {
    const url = new URL(request.url);
    const key = url.searchParams.get("key") || "default";
    const mode = url.searchParams.get("mode") || "write";

    if (mode === "read") {
      return Response.json({
        key,
        value: env.KV.get("waituntil:" + key),
      });
    }

    ctx.waitUntil(
      new Promise<void>((resolve) => {
        setTimeout(() => {
          env.KV.put("waituntil:" + key, new Date().toISOString());
          resolve();
        }, 1500);
      }),
    );

    return Response.json({
      queued: true,
      key,
      hint: "call the same URL with ?mode=read after ~1.5 s to see the timestamp",
    });
  },
} satisfies WorkersHandler;
