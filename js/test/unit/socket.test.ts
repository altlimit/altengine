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
  constructor(public url: string) {
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
