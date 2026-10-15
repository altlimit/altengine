import { createHmac } from "node:crypto";
import { describe, expect, it } from "vitest";
import { verifyWebhook, DEFAULT_WEBHOOK_TOLERANCE_MS } from "../../src/index.js";

// The reference signer is node:crypto, not the Web Crypto the helper uses, so a mistake shared by
// both halves of the test cannot pass it. The hosted service signs `v1=` + hex HMAC-SHA256 over
// `<timestamp>.<raw body>`, keyed with the whole secret string.
const SECRET = "whsec_3q2-7wEXAMPLEsecretVALUE";
const NOW = 1_760_000_000_000;

function sign(secret: string, ts: number | string, body: string | Buffer): string {
  return "v1=" + createHmac("sha256", secret).update(`${ts}.`).update(body).digest("hex");
}

const body = JSON.stringify({ type: "run.completed", run: { id: "r1", script: "invoices", error: "café" } });

describe("verifyWebhook", () => {
  it("accepts a signature the hosted service would send", async () => {
    expect(await verifyWebhook({ secret: SECRET, body, signature: sign(SECRET, NOW, body), timestamp: String(NOW), now: NOW })).toBe(true);
  });

  it("signs the raw bytes, whatever form they arrive in", async () => {
    const raw = Buffer.from(body, "utf8");
    const signature = sign(SECRET, NOW, raw);
    const u8 = new Uint8Array(raw);
    expect(await verifyWebhook({ secret: SECRET, body: u8, signature, timestamp: NOW, now: NOW })).toBe(true);
    expect(await verifyWebhook({ secret: SECRET, body: u8.slice().buffer, signature, timestamp: NOW, now: NOW })).toBe(true);
  });

  it("rejects a body that is not byte-for-byte what was signed", async () => {
    const signature = sign(SECRET, NOW, body);
    const reserialised = JSON.stringify(JSON.parse(body), null, 2);
    expect(await verifyWebhook({ secret: SECRET, body: reserialised, signature, timestamp: NOW, now: NOW })).toBe(false);
    expect(await verifyWebhook({ secret: SECRET, body: body + " ", signature, timestamp: NOW, now: NOW })).toBe(false);
  });

  it("is keyed with the whole secret, prefix included", async () => {
    const stripped = sign(SECRET.slice("whsec_".length), NOW, body);
    expect(await verifyWebhook({ secret: SECRET, body, signature: stripped, timestamp: NOW, now: NOW })).toBe(false);
    expect(await verifyWebhook({ secret: "whsec_other", body, signature: sign(SECRET, NOW, body), timestamp: NOW, now: NOW })).toBe(false);
  });

  it("rejects a timestamp that is not the one signed", async () => {
    const signature = sign(SECRET, NOW, body);
    expect(await verifyWebhook({ secret: SECRET, body, signature, timestamp: NOW + 1, now: NOW })).toBe(false);
  });

  it("rejects a stale or future timestamp, five minutes by default", async () => {
    const old = NOW - DEFAULT_WEBHOOK_TOLERANCE_MS - 1;
    const ahead = NOW + DEFAULT_WEBHOOK_TOLERANCE_MS + 1;
    const edge = NOW - DEFAULT_WEBHOOK_TOLERANCE_MS;
    expect(await verifyWebhook({ secret: SECRET, body, signature: sign(SECRET, old, body), timestamp: old, now: NOW })).toBe(false);
    expect(await verifyWebhook({ secret: SECRET, body, signature: sign(SECRET, ahead, body), timestamp: ahead, now: NOW })).toBe(false);
    expect(await verifyWebhook({ secret: SECRET, body, signature: sign(SECRET, edge, body), timestamp: edge, now: NOW })).toBe(true);
  });

  it("honours toleranceMs", async () => {
    const ts = NOW - 60_000;
    const signature = sign(SECRET, ts, body);
    expect(await verifyWebhook({ secret: SECRET, body, signature, timestamp: ts, now: NOW, toleranceMs: 30_000 })).toBe(false);
    expect(await verifyWebhook({ secret: SECRET, body, signature, timestamp: ts, now: NOW, toleranceMs: 120_000 })).toBe(true);
  });

  it("rejects missing or malformed headers without throwing", async () => {
    const signature = sign(SECRET, NOW, body);
    expect(await verifyWebhook({ secret: SECRET, body, signature: null, timestamp: NOW, now: NOW })).toBe(false);
    expect(await verifyWebhook({ secret: SECRET, body, signature, timestamp: undefined, now: NOW })).toBe(false);
    expect(await verifyWebhook({ secret: "", body, signature, timestamp: NOW, now: NOW })).toBe(false);
    expect(await verifyWebhook({ secret: SECRET, body, signature: signature.slice(0, -2), timestamp: NOW, now: NOW })).toBe(false);
    expect(await verifyWebhook({ secret: SECRET, body, signature: signature.slice(3), timestamp: NOW, now: NOW })).toBe(false);
    // A number in another notation names the same instant but is not the text that was signed.
    const sci = "1.76e12";
    expect(await verifyWebhook({ secret: SECRET, body, signature: sign(SECRET, sci, body), timestamp: sci, now: NOW })).toBe(false);
  });
});
