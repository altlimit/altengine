import { describe, expect, it, vi } from "vitest";
import { AltEngine, AuthClient, memoryStorage } from "../../src/index.js";

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });

const nowSec = () => Math.floor(Date.now() / 1000);

/** A signed-in token payload expiring `in` seconds from now. */
const tokens = (id: string, inSec = 3600) => ({
  id_token: id,
  refresh_token: "rt-" + id,
  expires_at: nowSec() + inSec,
  refresh_expires_at: nowSec() + 86400,
  user: { uid: "u1", identifier: "alice@example.com", claims: { name: "Alice" } },
});

function auth(fetchImpl: typeof fetch, opts: Record<string, unknown> = {}) {
  return new AuthClient({
    instance: "app-auth",
    baseUrl: "http://test.local",
    fetch: fetchImpl,
    storage: memoryStorage(), // never touch real localStorage in tests
    ...opts,
  });
}

describe("AuthClient", () => {
  it("signs in, stores the session, and exposes the user", async () => {
    const fetchMock = vi.fn(async (url: any, init: any) => {
      expect(String(url)).toBe("http://test.local/v1/auth/app-auth/signin");
      expect(init.method).toBe("POST");
      expect(JSON.parse(init.body)).toEqual({ identifier: "alice@example.com", password: "pw" });
      return json(200, tokens("id-1"));
    });
    const a = auth(fetchMock as any);
    const res = await a.signIn("alice@example.com", "pw");
    expect(res.status).toBe("signed_in");
    expect(a.isSignedIn).toBe(true);
    expect(a.user?.identifier).toBe("alice@example.com");
    expect(await a.getToken()).toBe("id-1");
  });

  it("surfaces a 2FA challenge instead of signing in", async () => {
    const fetchMock = vi.fn(async () => json(200, { mfa_required: true, mfa_token: "mfa-1" }));
    const a = auth(fetchMock as any);
    const res = await a.signIn("alice@example.com", "pw");
    expect(res).toEqual({ status: "mfa_required", mfaToken: "mfa-1" });
    // No tokens were issued — the user is NOT signed in until /2fa/verify.
    expect(a.isSignedIn).toBe(false);
    expect(await a.getToken()).toBeUndefined();
  });

  it("completes the 2FA challenge into a real session", async () => {
    const fetchMock = vi.fn(async (url: any, init: any) => {
      if (String(url).endsWith("/signin")) return json(200, { mfa_required: true, mfa_token: "mfa-1" });
      expect(String(url)).toBe("http://test.local/v1/auth/app-auth/2fa/verify");
      expect(JSON.parse(init.body)).toEqual({ mfa_token: "mfa-1", code: "123456" });
      return json(200, tokens("id-2"));
    });
    const a = auth(fetchMock as any);
    const res = await a.signIn("alice@example.com", "pw");
    if (res.status !== "mfa_required") throw new Error("expected a challenge");
    await a.verifyMfa(res.mfaToken, "123456");
    expect(a.isSignedIn).toBe(true);
    expect(await a.getToken()).toBe("id-2");
  });

  it("refreshes an expired id_token before handing it out", async () => {
    let calls = 0;
    const fetchMock = vi.fn(async (url: any, init: any) => {
      calls++;
      if (String(url).endsWith("/signin")) return json(200, tokens("stale", -10)); // already expired
      expect(String(url)).toBe("http://test.local/v1/auth/app-auth/token/refresh");
      expect(JSON.parse(init.body)).toEqual({ refresh_token: "rt-stale" });
      return json(200, { ...tokens("fresh"), user: undefined });
    });
    const a = auth(fetchMock as any);
    await a.signIn("alice@example.com", "pw");
    expect(await a.getToken()).toBe("fresh");
    expect(calls).toBe(2);
    // The user survives a refresh even though the refresh response has no `user`.
    expect(a.user?.uid).toBe("u1");
  });

  it("shares one refresh across concurrent callers", async () => {
    let refreshes = 0;
    const fetchMock = vi.fn(async (url: any) => {
      if (String(url).endsWith("/signin")) return json(200, tokens("stale", -10));
      refreshes++;
      return json(200, { ...tokens("fresh"), user: undefined });
    });
    const a = auth(fetchMock as any);
    await a.signIn("alice@example.com", "pw");
    const all = await Promise.all([a.getToken(), a.getToken(), a.getToken()]);
    expect(all).toEqual(["fresh", "fresh", "fresh"]);
    expect(refreshes).toBe(1); // single-flight, not three round trips
  });

  it("clears the session when the refresh token is rejected", async () => {
    const changes: (string | null)[] = [];
    const fetchMock = vi.fn(async (url: any) => {
      if (String(url).endsWith("/signin")) return json(200, tokens("stale", -10));
      return json(401, { error: { code: "UNAUTHENTICATED", message: "expired" } });
    });
    const a = auth(fetchMock as any);
    a.onChange((u) => changes.push(u ? u.uid : null));
    await a.signIn("alice@example.com", "pw");
    expect(await a.getToken()).toBeUndefined();
    expect(a.isSignedIn).toBe(false);
    expect(changes).toEqual(["u1", null]); // signed in, then signed out
  });

  it("signs out locally even when the server call fails", async () => {
    const fetchMock = vi.fn(async (url: any) => {
      if (String(url).endsWith("/signin")) return json(200, tokens("id-1"));
      throw new Error("network down");
    });
    const a = auth(fetchMock as any);
    await a.signIn("alice@example.com", "pw");
    await a.signOut();
    expect(a.isSignedIn).toBe(false);
    expect(a.user).toBeNull();
  });

  it("restores a session from storage across client instances", async () => {
    const store = memoryStorage();
    const fetchMock = vi.fn(async () => json(200, tokens("id-1")));
    const a = auth(fetchMock as any, { storage: store });
    await a.signIn("alice@example.com", "pw");

    // A fresh client (think: page reload) sees the same session.
    const b = auth(fetchMock as any, { storage: store });
    expect(b.isSignedIn).toBe(true);
    expect(b.user?.uid).toBe("u1");
    expect(await b.getToken()).toBe("id-1");
  });

  it("unwraps the { user } envelope from /me and refreshes the cached user", async () => {
    const fetchMock = vi.fn(async (url: any, init: any) => {
      if (String(url).endsWith("/signin")) return json(200, tokens("id-1"));
      expect(String(url)).toBe("http://test.local/v1/auth/app-auth/me");
      expect(init.headers.authorization).toBe("Bearer id-1");
      // The wire wraps the user — returning this envelope as-is would hand callers
      // an object with no `uid`.
      return json(200, { user: { uid: "u1", identifier: "alice@example.com", claims: { name: "Alice B" } } });
    });
    const a = auth(fetchMock as any);
    await a.signIn("alice@example.com", "pw");
    const me = await a.me();
    expect(me.uid).toBe("u1");
    expect(me.claims?.name).toBe("Alice B");
    expect(a.user?.claims?.name).toBe("Alice B"); // cached copy updated
  });

  it("never reveals whether an account exists on passwordless start", async () => {
    const fetchMock = vi.fn(async (url: any, init: any) => {
      expect(String(url)).toBe("http://test.local/v1/auth/app-auth/passwordless/start");
      expect(JSON.parse(init.body)).toEqual({ identifier: "nobody@example.com" });
      return json(200, { ok: true });
    });
    const a = auth(fetchMock as any);
    await expect(a.passwordlessStart("nobody@example.com")).resolves.toBeUndefined();
  });
});

describe("identity tokens on the data plane", () => {
  it("sends the user's id_token instead of the org API key", async () => {
    const authFetch = vi.fn(async () => json(200, tokens("id-user")));
    const a = auth(authFetch as any);
    await a.signIn("alice@example.com", "pw");

    const dataFetch = vi.fn(async (_url: any, init: any) => {
      expect(init.headers.authorization).toBe("Bearer id-user"); // NOT "Bearer org-key"
      return json(200, { documents: [], cursor: null });
    });
    const ae = new AltEngine({ baseUrl: "http://test.local", apiKey: "org-key", auth: a, fetch: dataFetch as any });
    await ae.datastore("app").query("posts");
    expect(dataFetch).toHaveBeenCalledOnce();
  });

  it("falls back to the API key when nobody is signed in", async () => {
    const a = auth(vi.fn() as any);
    const dataFetch = vi.fn(async (_url: any, init: any) => {
      expect(init.headers.authorization).toBe("Bearer org-key");
      return json(200, { documents: [], cursor: null });
    });
    const ae = new AltEngine({ baseUrl: "http://test.local", apiKey: "org-key", auth: a, fetch: dataFetch as any });
    await ae.datastore("app").query("posts");
  });

  it("re-reads the token each request, so a refresh is picked up mid-session", async () => {
    let signedIn = "id-1";
    const stub = { getToken: async () => signedIn };
    const seen: string[] = [];
    const dataFetch = vi.fn(async (_url: any, init: any) => {
      seen.push(init.headers.authorization);
      return json(200, { documents: [], cursor: null });
    });
    const ae = new AltEngine({ baseUrl: "http://test.local", auth: stub, fetch: dataFetch as any });
    await ae.datastore("app").query("posts");
    signedIn = "id-2"; // e.g. the client refreshed in the background
    await ae.datastore("app").query("posts");
    expect(seen).toEqual(["Bearer id-1", "Bearer id-2"]);
  });
});
