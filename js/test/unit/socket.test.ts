import { describe, expect, it } from "vitest";
import { ChannelSocket } from "../../src/channel/socket.js";

// A channel a limit refuses is answered in the ack's `refused` list rather than in `channels`.
// subscribe() used to wait for every channel it asked for to appear in `channels`, so a refusal
// left the promise pending until the socket closed.

class FakeWS {
  static last: FakeWS;
  readyState = 0;
  sent: string[] = [];
  onopen?: () => void;
  onmessage?: (ev: { data: string }) => void;
  onclose?: (ev: { code: number }) => void;
  onerror?: () => void;
  constructor(
    public url: string,
    public protocols?: string | string[]
  ) {
    FakeWS.last = this;
    queueMicrotask(() => {
      this.readyState = 1;
      this.onopen?.();
    });
  }
  send(s: string) {
    this.sent.push(s);
  }
  close() {
    this.readyState = 3;
  }
  frame(f: unknown) {
    this.onmessage?.({ data: JSON.stringify(f) });
  }
}

async function open() {
  const sock = new ChannelSocket({
    getToken: () => ({ token: "t", ws_url: "wss://x.test/v1/channel/i/subscribe" }),
    webSocket: FakeWS as unknown as new (url: string) => WebSocket,
    pingIntervalMs: 60_000,
  });
  await sock.connect();
  return sock;
}

describe("ChannelSocket refusals", () => {
  it("settles subscribe() with the admitted channels and reports the refused ones", async () => {
    const sock = await open();
    const refused: string[][] = [];
    sock.on("refused", (r) => refused.push(r));

    const p = sock.subscribe(["a", "b"]);
    FakeWS.last.frame({ type: "subscribed", channels: ["a"], refused: ["b"] });

    await expect(p).resolves.toEqual(["a"]);
    expect(refused).toEqual([["b"]]);
    sock.close();
  });

  it("reports a refusal at open, with no subscribe pending", async () => {
    const sock = await open();
    const refused: string[][] = [];
    sock.on("refused", (r) => refused.push(r));
    FakeWS.last.frame({ type: "subscribed", channels: [], refused: ["busy"] });
    expect(refused).toEqual([["busy"]]);
    sock.close();
  });
});

// The token goes in a subprotocol, not the URL, whenever the server says where to connect
// without it; a response with only `ws_url` comes from an older server and is opened as is.
describe("ChannelSocket token transport", () => {
  async function dial(tok: Record<string, unknown>, extra: Record<string, unknown> = {}) {
    const sock = new ChannelSocket({
      getToken: () => tok as any,
      webSocket: FakeWS as unknown as new (url: string, protocols?: string | string[]) => WebSocket,
      pingIntervalMs: 60_000,
      ...extra,
    });
    await sock.connect();
    sock.close();
    return FakeWS.last;
  }

  it("opens ws_endpoint with the response's protocols", async () => {
    const ws = await dial({
      token: "a.b.c",
      ws_endpoint: "wss://x.test/v1/channel/i/subscribe",
      protocols: ["altengine.v1", "altengine.token.a.b.c"],
      ws_url: "wss://x.test/v1/channel/i/subscribe?token=a.b.c",
    });
    expect(ws.url).toBe("wss://x.test/v1/channel/i/subscribe");
    expect(ws.protocols).toEqual(["altengine.v1", "altengine.token.a.b.c"]);
  });

  it("builds the protocols from the token when the response has an endpoint but no protocols", async () => {
    const ws = await dial({ token: "a.b.c", ws_endpoint: "wss://x.test/v1/channel/i/subscribe" });
    expect(ws.protocols).toEqual(["altengine.v1", "altengine.token.a.b.c"]);
  });

  it("falls back to ws_url, token in the query, for an older server", async () => {
    const ws = await dial({ token: "a.b.c", ws_url: "wss://x.test/v1/channel/i/subscribe?token=a.b.c" });
    expect(ws.url).toBe("wss://x.test/v1/channel/i/subscribe?token=a.b.c");
    expect(ws.protocols).toBeUndefined();
  });

  it("sends the token as a subprotocol to an explicit url", async () => {
    const ws = await dial({ token: "a.b.c" }, { url: "wss://x.test/v1/channel/i/subscribe", channels: ["r1", "r2"] });
    expect(ws.url).toBe("wss://x.test/v1/channel/i/subscribe?channels=r1%2Cr2");
    expect(ws.url).not.toContain("a.b.c");
    expect(ws.protocols).toEqual(["altengine.v1", "altengine.token.a.b.c"]);
  });
});
