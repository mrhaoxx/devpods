import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiFailure, req } from "./api";

describe("req", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("throws ApiFailure carrying the body's code", async () => {
    vi.stubGlobal("fetch", async () => new Response(JSON.stringify({ code: "QUOTA_EXCEEDED", message: "too much" }), { status: 409 }));
    const err = (await req("POST", "/api/devpods", {}).catch((e: unknown) => e)) as ApiFailure;
    expect(err).toBeInstanceOf(ApiFailure);
    expect(err.status).toBe(409);
    expect(err.code).toBe("QUOTA_EXCEEDED");
    expect(err.message).toBe("too much");
  });

  it("a non-JSON error body still yields an ApiFailure", async () => {
    vi.stubGlobal("fetch", async () => new Response("<html>bad gateway</html>", { status: 502 }));
    const err = (await req("GET", "/api/me").catch((e: unknown) => e)) as ApiFailure;
    expect(err).toBeInstanceOf(ApiFailure);
    expect(err.code).toBe("HTTP_502");
  });
});
