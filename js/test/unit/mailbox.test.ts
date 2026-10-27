import { describe, expect, it, vi } from "vitest";
import { AltEngine, attachmentBase64 } from "../../src/index.js";

// The mailbox client is thin over REST. What is worth asserting is what a caller would get wrong
// by hand: bytes become base64, a send is never retried, a wait is given longer than the server
// holds it, and the paths and queries are the documented ones.

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });

function client(fetchImpl: typeof fetch) {
  return new AltEngine({ baseUrl: "http://test.local", apiKey: "k", fetch: fetchImpl }).mailbox("desk");
}

describe("attachments", () => {
  it("encodes bytes, buffers, views and blobs; passes base64 text through", async () => {
    const hi = new TextEncoder().encode("hi");
    expect(await attachmentBase64("aGk=")).toBe("aGk=");
    expect(await attachmentBase64(hi)).toBe("aGk=");
    expect(await attachmentBase64(hi.buffer)).toBe("aGk=");
    expect(await attachmentBase64(new DataView(new Uint8Array([0, 104, 105]).buffer, 1))).toBe("aGk=");
    expect(await attachmentBase64(new Blob(["hi"]))).toBe("aGk=");
    // Larger than one String.fromCharCode chunk.
    const big = new Uint8Array(100_000).fill(65);
    expect(Buffer.from(await attachmentBase64(big), "base64").equals(Buffer.from(big))).toBe(true);
  });

  it("sends them base64'd and never retries a send", async () => {
    let calls = 0;
    const fetchMock = vi.fn(async (url: any, init: any) => {
      calls++;
      expect(String(url)).toBe("http://test.local/v1/mailbox/desk/send");
      expect(JSON.parse(init.body)).toEqual({
        to: ["a@example.com"],
        subject: "s",
        text: "t",
        attachments: [{ filename: "a.txt", content: "aGk=" }, { content: "eA==", contentId: "logo" }],
      });
      return json(503, { error: { code: "UNAVAILABLE", message: "try later" } });
    });
    await expect(
      client(fetchMock as never).send({
        to: ["a@example.com"],
        subject: "s",
        text: "t",
        attachments: [{ filename: "a.txt", content: new TextEncoder().encode("hi") }, { content: "eA==", contentId: "logo" }],
      })
    ).rejects.toThrow(/try later/);
    expect(calls).toBe(1);
  });

  it("omits attachments from a send that has none", async () => {
    const fetchMock = vi.fn(async (_url: any, init: any) => {
      expect(JSON.parse(init.body)).toEqual({ text: "ok" });
      return json(201, { message: { id: "m2" } });
    });
    expect((await client(fetchMock as never).reply("m1", { text: "ok" })).id).toBe("m2");
    expect(String(fetchMock.mock.calls[0]![0])).toBe("http://test.local/v1/mailbox/desk/messages/m1/reply");
  });
});

describe("reading", () => {
  it("lists with the documented query", async () => {
    const fetchMock = vi.fn(async (url: any) => {
      const u = new URL(String(url));
      expect(u.pathname).toBe("/v1/mailbox/desk/messages");
      expect(Object.fromEntries(u.searchParams)).toEqual({ to: "support", direction: "in", unread: "1", limit: "10" });
      return json(200, { messages: [], cursor: null });
    });
    expect(await client(fetchMock as never).list({ to: "support", direction: "in", unread: true, limit: 10 })).toEqual({
      messages: [],
      cursor: null,
    });
  });

  it("caps a wait at 30 seconds and gives the request longer than that", async () => {
    const fetchMock = vi.fn(async (url: any, init: any) => {
      const u = new URL(String(url));
      expect(u.searchParams.get("wait")).toBe("30");
      expect(u.searchParams.get("after")).toBe("now");
      // The client's own 10ms timeout must not cut a long-poll short.
      await new Promise((r) => setTimeout(r, 50));
      expect(init.signal.aborted).toBe(false);
      return json(200, { messages: [], cursor: "now" });
    });
    const out = await new AltEngine({ baseUrl: "http://test.local", apiKey: "k", fetch: fetchMock as never, timeoutMs: 10 })
      .mailbox("desk")
      .wait({ after: "now", timeoutSeconds: 120 });
    expect(out.cursor).toBe("now");
  });

  it("returns the raw link and the attachment link", async () => {
    const fetchMock = vi.fn(async (url: any) => {
      const u = new URL(String(url));
      if (u.pathname.endsWith("/attachments/2")) return json(200, { url: "http://x/a", filename: "f", contentType: "text/plain", size: 1, expiresAt: 5 });
      expect(u.searchParams.get("raw")).toBe("1");
      return json(200, { message: { id: "m1" }, rawUrl: "http://x/raw", expiresAt: 9 });
    });
    const mb = client(fetchMock as never);
    expect(await mb.raw("m1")).toEqual({ url: "http://x/raw", expiresAt: 9 });
    expect((await mb.attachment("m1", 2)).url).toBe("http://x/a");
  });
});

describe("keeping and addresses", () => {
  it("posts days to /keep and unwraps the summary", async () => {
    const fetchMock = vi.fn(async (url: any, init: any) => {
      expect(String(url)).toBe("http://test.local/v1/mailbox/desk/messages/m%2F1/keep");
      expect(JSON.parse(init.body)).toEqual({ days: 30 });
      return json(200, { message: { id: "m/1", expiresAt: 123 } });
    });
    expect((await client(fetchMock as never).keep("m/1", 30)).expiresAt).toBe(123);
  });

  it("maps addresses to their routes", async () => {
    const seen: string[] = [];
    const fetchMock = vi.fn(async (url: any, init: any) => {
      seen.push(`${init.method} ${new URL(String(url)).pathname}`);
      return json(200, { addresses: [], cursor: null, local: "x" });
    });
    const mb = client(fetchMock as never);
    await mb.addresses.list({ limit: 5 });
    await mb.addresses.create({ label: "minted" });
    await mb.addresses.disable("support");
    expect(seen).toEqual(["GET /v1/mailbox/desk/addresses", "POST /v1/mailbox/desk/addresses", "DELETE /v1/mailbox/desk/addresses/support"]);
  });
});
