/** Verifying an automation completion webhook.
 *
 * Every webhook carries two headers:
 *
 * - `x-altengine-timestamp` — when it was sent, in milliseconds since the epoch.
 * - `x-altengine-signature` — `v1=<hex>`, an HMAC-SHA256 of `<timestamp>.<raw body>` keyed with
 *   the instance's whole webhook signing secret, `whsec_` prefix included.
 *
 * Uses Web Crypto, so it runs the same on Node 20+, in a browser, and in any edge runtime. */

export const WEBHOOK_SIGNATURE_HEADER = "x-altengine-signature";
export const WEBHOOK_TIMESTAMP_HEADER = "x-altengine-timestamp";

/** How far the timestamp may be from now, either way, by default: five minutes. */
export const DEFAULT_WEBHOOK_TOLERANCE_MS = 5 * 60 * 1000;

export interface VerifyWebhookOptions {
  /** The instance's webhook signing secret, `whsec_…`, as the console or MCP shows it. */
  secret: string;
  /** The body exactly as received — NOT a re-serialised parse, which signs different bytes. */
  body: string | Uint8Array | ArrayBuffer;
  /** The `x-altengine-signature` header. */
  signature: string | null | undefined;
  /** The `x-altengine-timestamp` header. */
  timestamp: string | number | null | undefined;
  /** Reject a timestamp further than this from now, either way. Default five minutes. */
  toleranceMs?: number;
  /** The current time in ms. For tests; defaults to `Date.now()`. */
  now?: number;
}

const enc = new TextEncoder();

function bytesOf(body: string | Uint8Array | ArrayBuffer): Uint8Array {
  if (typeof body === "string") return enc.encode(body);
  return body instanceof Uint8Array ? body : new Uint8Array(body);
}

function hex(buf: ArrayBuffer): string {
  let s = "";
  for (const b of new Uint8Array(buf)) s += b.toString(16).padStart(2, "0");
  return s;
}

/** Compares every byte whatever the first mismatch, so timing does not reveal how much matched. */
function constantTimeEqual(a: string, b: string): boolean {
  const x = enc.encode(a);
  const y = enc.encode(b);
  if (x.length !== y.length) return false;
  let diff = 0;
  for (let i = 0; i < x.length; i++) diff |= x[i]! ^ y[i]!;
  return diff === 0;
}

/**
 * True when the webhook was signed with `secret` and its timestamp is within `toleranceMs` of now.
 *
 * ```ts
 * import { verifyWebhook } from "@altengine/sdk";
 *
 * const ok = await verifyWebhook({
 *   secret: process.env.ALTENGINE_WEBHOOK_SECRET!,
 *   body: await request.text(),
 *   signature: request.headers.get("x-altengine-signature"),
 *   timestamp: request.headers.get("x-altengine-timestamp"),
 * });
 * if (!ok) return new Response("bad signature", { status: 401 });
 * ```
 *
 * Each delivery attempt is signed afresh, so a retry carries a new timestamp: deduplicate on
 * `run.id`, not on the signature.
 */
export async function verifyWebhook(opts: VerifyWebhookOptions): Promise<boolean> {
  const { secret, signature } = opts;
  if (!secret || !signature) return false;
  const ts = opts.timestamp == null ? "" : String(opts.timestamp).trim();
  // Digits only: the timestamp is signed as the header's own text, and Number() would accept
  // "1e12" or "0x1f" as a time while the signed string differs.
  if (!/^\d+$/.test(ts)) return false;
  const tolerance = opts.toleranceMs ?? DEFAULT_WEBHOOK_TOLERANCE_MS;
  const now = opts.now ?? Date.now();
  if (!(Math.abs(now - Number(ts)) <= tolerance)) return false;

  const key = await crypto.subtle.importKey("raw", enc.encode(secret), { name: "HMAC", hash: "SHA-256" }, false, [
    "sign",
  ]);
  const prefix = enc.encode(`${ts}.`);
  const body = bytesOf(opts.body);
  const signed = new Uint8Array(prefix.length + body.length);
  signed.set(prefix, 0);
  signed.set(body, prefix.length);
  const expected = `v1=${hex(await crypto.subtle.sign("HMAC", key, signed))}`;
  return constantTimeEqual(signature.trim(), expected);
}
