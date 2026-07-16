import { AltEngineError, AltEngineNetworkError } from "./errors.js";

export interface RetryOptions {
  /** Total attempts including the first (default 3). Set 1 to disable retries. */
  maxAttempts?: number;
  /** Base backoff delay in ms (default 250); doubles per attempt with jitter. */
  baseDelayMs?: number;
  /** Backoff ceiling in ms (default 4000). */
  maxDelayMs?: number;
}

/** Production API origin used when no override is given. */
export const DEFAULT_BASE_URL = "https://api.altengine.net";
/** Where `altengine dev` (the local emulator) listens by default. */
export const DEV_BASE_URL = "http://127.0.0.1:9191";

export interface ClientOptions {
  /** API origin override. Resolution order: this option → `dev: true` →
   * `ALTENGINE_URL` env var → `https://api.altengine.net` (production). */
  baseUrl?: string;
  /** Target the local emulator (`altengine dev`) at `http://127.0.0.1:9191`. */
  dev?: boolean;
  /** Org API key (`Authorization: Bearer`). Falls back to the `ALTENGINE_API_KEY`
   * env var; omit only for token-based flows. */
  apiKey?: string;
  /** Custom fetch (tests, polyfills). Defaults to `globalThis.fetch`. */
  fetch?: typeof globalThis.fetch;
  /** Per-request timeout in ms (default 30000). */
  timeoutMs?: number;
  /** Default retry policy for retryable failures (429/502/503/504/network). */
  retry?: RetryOptions;
}

/** Browser-safe env lookup (process is Node/edge-only). */
const env = (name: string): string | undefined =>
  typeof process !== "undefined" ? process.env?.[name] : undefined;

export function resolveBaseUrl(opts: ClientOptions): string {
  if (opts.baseUrl) return opts.baseUrl;
  if (opts.dev) return DEV_BASE_URL;
  return env("ALTENGINE_URL") ?? DEFAULT_BASE_URL;
}

export interface RequestOptions {
  query?: Record<string, string | number | boolean | undefined>;
  body?: unknown;
  headers?: Record<string, string>;
  /** Override the client retry policy; `false` disables retries for this call
   * (used for non-idempotent endpoints: transaction, publish). */
  retry?: RetryOptions | false;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/** Minimal JSON-over-fetch transport shared by all service clients. */
export class Http {
  private readonly baseUrl: string;
  private readonly apiKey?: string;
  private readonly fetchImpl: typeof globalThis.fetch;
  private readonly timeoutMs: number;
  private readonly retry: Required<RetryOptions>;

  constructor(opts: ClientOptions = {}) {
    this.baseUrl = resolveBaseUrl(opts).replace(/\/+$/, "");
    this.apiKey = opts.apiKey ?? env("ALTENGINE_API_KEY");
    this.fetchImpl = opts.fetch ?? globalThis.fetch;
    if (!this.fetchImpl) throw new Error("no fetch implementation available; pass { fetch }");
    this.timeoutMs = opts.timeoutMs ?? 30_000;
    this.retry = {
      maxAttempts: opts.retry?.maxAttempts ?? 3,
      baseDelayMs: opts.retry?.baseDelayMs ?? 250,
      maxDelayMs: opts.retry?.maxDelayMs ?? 4_000,
    };
  }

  url(path: string, query?: RequestOptions["query"]): string {
    let url = this.baseUrl + path;
    if (query) {
      const qs = new URLSearchParams();
      for (const [k, v] of Object.entries(query)) {
        if (v !== undefined) qs.set(k, String(v));
      }
      const s = qs.toString();
      if (s) url += "?" + s;
    }
    return url;
  }

  async request<T>(method: string, path: string, opts: RequestOptions = {}): Promise<T> {
    const retry = opts.retry === false
      ? { maxAttempts: 1, baseDelayMs: 0, maxDelayMs: 0 }
      : { ...this.retry, ...(opts.retry ?? {}) };

    let lastErr: unknown;
    for (let attempt = 1; ; attempt++) {
      try {
        return await this.once<T>(method, path, opts);
      } catch (err) {
        lastErr = err;
        const retryable =
          err instanceof AltEngineNetworkError ||
          (err instanceof AltEngineError && err.retryable);
        if (!retryable || attempt >= retry.maxAttempts) throw err;
        let delay = Math.min(retry.baseDelayMs * 2 ** (attempt - 1), retry.maxDelayMs);
        if (err instanceof AltEngineError && err.retryAfter !== undefined) {
          delay = Math.max(delay, err.retryAfter * 1000);
        }
        // Full jitter keeps concurrent retries from stampeding in unison.
        await sleep(delay * (0.5 + Math.random() * 0.5));
      }
    }
    // Unreachable, but satisfies control-flow analysis.
    throw lastErr;
  }

  private async once<T>(method: string, path: string, opts: RequestOptions): Promise<T> {
    const headers: Record<string, string> = { ...opts.headers };
    if (this.apiKey) headers["authorization"] = `Bearer ${this.apiKey}`;
    let body: string | undefined;
    if (opts.body !== undefined) {
      headers["content-type"] = "application/json";
      body = JSON.stringify(opts.body);
    }

    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), this.timeoutMs);
    let res: Response;
    try {
      res = await this.fetchImpl(this.url(path, opts.query), { method, headers, body, signal: ctrl.signal });
    } catch (err) {
      throw new AltEngineNetworkError(`request failed: ${method} ${path}`, err);
    } finally {
      clearTimeout(timer);
    }

    if (!res.ok) throw await toApiError(res);
    if (res.status === 204) return undefined as T;
    return (await res.json()) as T;
  }
}

async function toApiError(res: Response): Promise<AltEngineError> {
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
    // Non-JSON error body; keep the status-derived defaults.
  }
  const ra = res.headers.get("retry-after");
  const retryAfter = ra !== null && !Number.isNaN(Number(ra)) ? Number(ra) : undefined;
  return new AltEngineError({ code, message, status: res.status, details, retryAfter });
}

/** Encode one path segment (instance/index/collection/key names). */
export const seg = (s: string | number): string => encodeURIComponent(String(s));
