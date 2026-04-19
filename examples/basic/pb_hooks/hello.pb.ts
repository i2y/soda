export default {
  route: "/api/hello",

  async fetch(_request: Request, _env: Env): Promise<Response> {
    return Response.json({
      message: "Hello from Soda!",
      runtime: "Ramune + upstream PocketBase",
    });
  },
} satisfies WorkersHandler;
