// Web Crypto demo — demonstrates crypto.subtle, crypto.randomUUID,
// crypto.getRandomValues working inside a Workers-style fetch handler.
//
//   curl http://127.0.0.1:8094/api/crypto
export default {
  route: "/api/crypto",

  async fetch(_request: Request, _env: Env): Promise<Response> {
    // randomUUID — version 4 UUID from a CSPRNG
    const uuid = crypto.randomUUID();

    // subtle.digest — SHA-256 of "hello" ought to be 2cf24dba...
    const digest = await crypto.subtle.digest(
      "SHA-256",
      new TextEncoder().encode("hello"),
    );
    const digestHex = [...new Uint8Array(digest)]
      .map((b) => b.toString(16).padStart(2, "0"))
      .join("");

    // getRandomValues — fills a typed array with cryptographic bytes
    const randomBytes = new Uint8Array(16);
    crypto.getRandomValues(randomBytes);

    return Response.json({
      uuid,
      digest_sha256_of_hello: digestHex,
      expected_digest: "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
      random_bytes: Array.from(randomBytes),
    });
  },
} satisfies WorkersHandler;
