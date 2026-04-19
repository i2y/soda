// Response streaming demo — returns a Response whose body is a
// ReadableStream. Soda's Workers handler drives the stream chunk by
// chunk and flushes each chunk to the client immediately, so the
// endpoint behaves like a Server-Sent Events source.
//
//   curl -N http://127.0.0.1:8094/api/sse
export default {
  route: "/api/sse",

  async fetch(_request: Request, _env: Env): Promise<Response> {
    const encoder = new TextEncoder();
    const stream = new ReadableStream({
      async start(controller) {
        for (let i = 1; i <= 5; i++) {
          const line = `data: tick ${i} at ${new Date().toISOString()}\n\n`;
          controller.enqueue(encoder.encode(line));
          if (i < 5) await new Promise((r) => setTimeout(r, 400));
        }
        controller.close();
      },
    });

    return new Response(stream, {
      headers: {
        "Content-Type": "text/event-stream",
        "Cache-Control": "no-cache",
      },
    });
  },
} satisfies WorkersHandler;
