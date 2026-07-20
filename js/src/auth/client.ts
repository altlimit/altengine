/** AuthClient — the browser-side identity client for `/v1/auth/{instance}`.
 *
 * This is the piece that lets a client-side app talk to altengine with **no
 * backend of its own**: the end user signs in here, and the resulting `id_token`
 * is what Datastore/Channel requests carry. Access rules on the auth instance
 * decide what that user may read and write.
 *
 * Deliberately self-contained and API-key-free (like `ChannelSocket`) so it is
 * safe to ship to a browser — see the `@altengine/sdk/auth` subpath export.
 *
 * ```ts
 * import { AuthClient } from "@altengine/sdk/auth";
 * import { AltEngine } from "@altengine/sdk";
 *
 * const auth = new AuthClient({ instance: "myapp-auth" });
 * await auth.signIn("alice@example.com", "hunter2");
 *
 * // Hand the client to the SDK: every request now carries the user's token.
 * const ae = new AltEngine({ auth });
 * const { documents } = await ae.datastore("myapp").query("posts", { limit: 20 });
 * ```
 */

import { AltEngineError, AltEngineNetworkError } from "../errors.js";
import { DEFAULT_BASE_URL, DEV_BASE_URL, seg } from "../constants.js";
import { defaultStorage, memoryStorage, type TokenStorage } from "./storage.js";
import type { AuthConfig, AuthSession, AuthUser, SignInResult, StoredSession, AuthTokens } from "./types.js";

export interface AuthClientOptions {
  /** The auth instance name, e.g. "myapp-auth". */
  instance: string;
  /** API origin override. Resolution order: this → `dev: true` → production. */
  baseUrl?: string;
  /** Target the local emulator (`altengine dev`) at `http://127.0.0.1:9191`. */
  dev?: boolean;
  /** Where to persist the session (default: `localStorage`, else in-memory).
   * Pass `memoryStorage()` to keep the session out of storage entirely. */
  storage?: TokenStorage;
  /** Storage key prefix, so several apps/instances can share one origin.
   * Defaults to `altengine.auth.<instance>`. */
  storageKey?: string;
  /** Custom fetch (tests, polyfills). Defaults to `globalThis.fetch`. */
  fetch?: typeof globalThis.fetch;
  /** Refresh the id_token this many seconds before it expires (default 30). */
  refreshSkewSeconds?: number;
}

type Listener = (user: AuthUser | null) => void;

const nowSec = () => Math.floor(Date.now() / 1000);

export class AuthClient {
  readonly instance: string;
  readonly baseUrl: string;

  private readonly base: string;
  private readonly storage: TokenStorage;
  private readonly key: string;
  private readonly fetchImpl: typeof globalThis.fetch;
  private readonly skew: number;
  private readonly listeners = new Set<Listener>();

  private session: StoredSession | null;
  /** In-flight refresh, so concurrent requests share one round trip. */
  private refreshing: Promise<string | undefined> | null = null;

  constructor(opts: AuthClientOptions) {
    if (!opts.instance) throw new Error("AuthClient requires an `instance`");
    this.instance = opts.instance;
    this.baseUrl = (opts.baseUrl ?? (opts.dev ? DEV_BASE_URL : DEFAULT_BASE_URL)).replace(/\/+$/, "");
    this.base = `${this.baseUrl}/v1/auth/${seg(opts.instance)}`;
    this.storage = opts.storage ?? defaultStorage();
    this.key = opts.storageKey ?? `altengine.auth.${opts.instance}`;
    const f = opts.fetch ?? globalThis.fetch;
    if (!f) throw new Error("no fetch implementation available; pass { fetch }");
    this.fetchImpl = f;
    this.skew = opts.refreshSkewSeconds ?? 30;
    this.session = this.load();
  }

  // --- session state ------------------------------------------------------

  /** The signed-in user, or null. Reads persisted state, so it survives reloads. */
  get user(): AuthUser | null {
    return this.session?.user ?? null;
  }

  get isSignedIn(): boolean {
    return !!this.session?.id_token;
  }

  /** Subscribe to sign-in/sign-out. Returns an unsubscribe function. */
  onChange(fn: Listener): () => void {
    this.listeners.add(fn);
    return () => void this.listeners.delete(fn);
  }

  private emit(): void {
    for (const fn of this.listeners) {
      try {
        fn(this.user);
      } catch {
        /* a listener must never break the auth flow */
      }
    }
  }

  private load(): StoredSession | null {
    try {
      const raw = this.storage.get(this.key);
      return raw ? (JSON.parse(raw) as StoredSession) : null;
    } catch {
      return null;
    }
  }

  private save(s: StoredSession | null): void {
    this.session = s;
    try {
      if (s) this.storage.set(this.key, JSON.stringify(s));
      else this.storage.remove(this.key);
    } catch {
      /* storage full / blocked — the session still works for this page */
    }
    this.emit();
  }

  private adopt(t: AuthTokens & { user?: AuthUser }): AuthSession {
    const user = t.user ?? this.session?.user ?? null;
    this.save({ id_token: t.id_token, refresh_token: t.refresh_token, expires_at: t.expires_at, user });
    return { ...t, user: user as AuthUser };
  }

  // --- transport ----------------------------------------------------------

  private async call<T>(method: string, path: string, body?: unknown, bearer?: string): Promise<T> {
    const headers: Record<string, string> = {};
    if (bearer) headers["authorization"] = `Bearer ${bearer}`;
    if (body !== undefined) headers["content-type"] = "application/json";
    let res: Response;
    try {
      res = await this.fetchImpl(this.base + path, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
      });
    } catch (err) {
      throw new AltEngineNetworkError(`request failed: ${method} ${path}`, err);
    }
    if (!res.ok) throw await toError(res);
    if (res.status === 204) return undefined as T;
    return (await res.json()) as T;
  }

  // --- config -------------------------------------------------------------

  /** The instance's public sign-in configuration — which fields the sign-up form
   * collects, which one is the login identity, and which methods are enabled.
   * Fetch this to render a sign-in UI that matches the instance's settings. */
  async config(): Promise<AuthConfig> {
    return this.call<AuthConfig>("GET", "/config");
  }

  // --- sign up / in / out -------------------------------------------------

  /** Create an account. Pass the configured sign-up fields plus `password`,
   * e.g. `{ email, name, password }`. Signs the new user in. */
  async signUp(fields: Record<string, unknown>): Promise<AuthSession> {
    const t = await this.call<AuthSession>("POST", "/signup", fields);
    return this.adopt(t);
  }

  /** Sign in with the identity field + password. When the account has two-factor
   * enabled this resolves to `{ status: "mfa_required" }` and you must call
   * {@link verifyMfa} with the code — no tokens are issued until then. */
  async signIn(identifier: string, password: string): Promise<SignInResult> {
    const t = await this.call<AuthSession | { mfa_required: true; mfa_token: string }>("POST", "/signin", {
      identifier,
      password,
    });
    if ("mfa_required" in t) return { status: "mfa_required", mfaToken: t.mfa_token };
    return { status: "signed_in", session: this.adopt(t) };
  }

  /** Complete a two-factor challenge from {@link signIn} or {@link passwordlessVerify}. */
  async verifyMfa(mfaToken: string, code: string): Promise<AuthSession> {
    const t = await this.call<AuthSession>("POST", "/2fa/verify", { mfa_token: mfaToken, code });
    return this.adopt(t);
  }

  /** Start passwordless sign-in: emails a one-time code. Always resolves — the
   * server never reveals whether the account exists. */
  async passwordlessStart(identifier: string): Promise<void> {
    await this.call("POST", "/passwordless/start", { identifier });
  }

  /** Finish passwordless sign-in with the emailed code. */
  async passwordlessVerify(identifier: string, code: string): Promise<SignInResult> {
    const t = await this.call<AuthSession | { mfa_required: true; mfa_token: string }>("POST", "/passwordless/verify", {
      identifier,
      code,
    });
    if ("mfa_required" in t) return { status: "mfa_required", mfaToken: t.mfa_token };
    return { status: "signed_in", session: this.adopt(t) };
  }

  /** Start a password reset: emails a one-time code. Always resolves. */
  async passwordResetStart(identifier: string): Promise<void> {
    await this.call("POST", "/password/reset/start", { identifier });
  }

  /** Finish a password reset with the emailed code and a new password. */
  async passwordResetVerify(identifier: string, code: string, password: string): Promise<void> {
    await this.call("POST", "/password/reset/verify", { identifier, code, password });
  }

  /** The signed-in user, re-read from the server (picks up claim changes). */
  async me(): Promise<AuthUser> {
    const tok = await this.getToken();
    if (!tok) throw new AltEngineError({ code: "UNAUTHENTICATED", message: "not signed in", status: 401 });
    // The wire wraps the user in a `{ user }` envelope.
    const { user } = await this.call<{ user: AuthUser }>("GET", "/me", undefined, tok);
    // Keep the cached user in step, so claim changes made elsewhere land locally too.
    if (user && this.session) this.save({ ...this.session, user });
    return user;
  }

  /** Revoke the refresh token server-side and clear local state. Always clears
   * locally, even if the network call fails. */
  async signOut(): Promise<void> {
    const rt = this.session?.refresh_token;
    this.save(null);
    if (rt) {
      try {
        await this.call("POST", "/signout", { refresh_token: rt });
      } catch {
        /* best-effort: local state is already gone */
      }
    }
  }

  // --- tokens -------------------------------------------------------------

  /** Force a refresh-token rotation and return the new id_token. */
  async refresh(): Promise<string> {
    const rt = this.session?.refresh_token;
    if (!rt) throw new AltEngineError({ code: "UNAUTHENTICATED", message: "not signed in", status: 401 });
    const t = await this.call<AuthTokens>("POST", "/token/refresh", { refresh_token: rt });
    this.adopt(t);
    return t.id_token;
  }

  /**
   * The current id_token, refreshed first if it is expired or about to be.
   * Returns `undefined` when nobody is signed in. This is the `TokenProvider`
   * the SDK calls before every Datastore/Search/Channel request — pass the
   * client as `new AltEngine({ auth })` and tokens stay fresh automatically.
   *
   * Concurrent callers share a single refresh. If the refresh fails because the
   * session is truly gone (401/403), local state is cleared so the app can react
   * via {@link onChange} instead of retrying forever.
   */
  async getToken(): Promise<string | undefined> {
    const s = this.session;
    if (!s?.id_token) return undefined;
    if (s.expires_at - this.skew > nowSec()) return s.id_token;
    if (!this.refreshing) {
      this.refreshing = this.refresh()
        .catch((err) => {
          if (err instanceof AltEngineError && (err.status === 401 || err.status === 403)) {
            this.save(null); // refresh token revoked/expired — session is over
            return undefined;
          }
          throw err; // transient (network/5xx): let the caller see it and retry
        })
        .finally(() => {
          this.refreshing = null;
        });
    }
    return this.refreshing;
  }
}

async function toError(res: Response): Promise<AltEngineError> {
  let code = "INTERNAL";
  let message = `HTTP ${res.status}`;
  let details: unknown;
  try {
    const parsed = (await res.json()) as { error?: { code?: string; message?: string; details?: unknown } };
    if (parsed?.error) {
      code = parsed.error.code ?? code;
      message = parsed.error.message ?? message;
      details = parsed.error.details;
    }
  } catch {
    /* non-JSON error body */
  }
  const ra = res.headers.get("retry-after");
  const retryAfter = ra !== null && !Number.isNaN(Number(ra)) ? Number(ra) : undefined;
  return new AltEngineError({ code, message, status: res.status, details, retryAfter });
}

export { memoryStorage, defaultStorage, type TokenStorage };
