import { beforeAll, describe, expect, it } from "vitest";
import WebSocket from "ws";
import type { ChannelClient, ChannelMessage } from "../../src/index.js";
import { ChannelSocket } from "../../src/channel/socket.js";
import { client, uniq } from "./helpers.js";

const instance = uniq("sdk-conf");
let ch: ChannelClient;

// Node's `ws` implements the browser WebSocket surface closely enough for the socket.
const wsCtor = WebSocket as unknown as new (url: string) => globalThis.WebSocket;

const waitFor = <T>(fn: (resolve: (v: T) => void) => void, ms = 10_000): Promise<T> =>
  new Promise<T>((resolve, reject) => {
    const t = setTimeout(() => reject(new Error("timed out waiting")), ms);
    fn((v) => {
      clearTimeout(t);
      resolve(v);
    });
  });

beforeAll(() => {
  ch = client().channel(instance);
});

describe("channel tokens & publish", () => {
  it("mints a token with ws_url and echoes channels", async () => {
    const tok = await ch.createToken({ channels: ["room:1", "room:2"], ttl_seconds: 600 });
    expect(tok.token.split(".")).toHaveLength(3);
    expect(tok.channels).toEqual(["room:1", "room:2"]);
    expect(tok.ws_url).toContain("/subscribe");
    expect(tok.expires_at).toBeGreaterThan(Date.now() / 1000);
  });

  it("HTTP publish reports delivered count (0 with no subscribers)", async () => {
    const res = await ch.publish("room:empty", { hello: 1 });
    expect(res.delivered).toBe(0);
  });
});

describe("channel WebSocket lifecycle", () => {
  it("subscribes and receives an HTTP-published message", async () => {
    const sock = new ChannelSocket({
      getToken: () => ch.createToken({ channels: ["room:live"] }),
      webSocket: wsCtor,
    });
    try {
      await sock.connect();
      expect(sock.state).toBe("open");
      const got = waitFor<ChannelMessage>((resolve) => sock.on("message", resolve));
      // Publish may race the subscribe registration; retry until delivered.
      for (let i = 0; i < 50; i++) {
        const { delivered } = await ch.publish("room:live", { n: i });
        if (delivered > 0) break;
        await new Promise((r) => setTimeout(r, 100));
      }
      const msg = await got;
      expect(msg.channel).toBe("room:live");
      expect((msg.data as { n: number }).n).toBeGreaterThanOrEqual(0);
      expect(msg.ts).toBeGreaterThan(0);
    } finally {
      sock.close();
    }
  });

  it("publishes over the socket with a ws-capable token", async () => {
    const a = new ChannelSocket({
      getToken: () => ch.createToken({ channels: ["room:ws"], publish: "ws" }),
      webSocket: wsCtor,
    });
    const b = new ChannelSocket({
      getToken: () => ch.createToken({ channels: ["room:ws"] }),
      webSocket: wsCtor,
    });
    try {
      await a.connect();
      await b.connect();
      const got = waitFor<ChannelMessage>((resolve) => b.on("message", resolve));
      // Retry until b's subscription is registered server-side.
      let delivered = 0;
      for (let i = 0; i < 50 && delivered < 2; i++) {
        delivered = await a.publish("room:ws", { via: "ws" });
        if (delivered < 2) await new Promise((r) => setTimeout(r, 100));
      }
      expect(delivered).toBeGreaterThanOrEqual(2); // a + b are both subscribed
      const msg = await got;
      expect((msg.data as { via: string }).via).toBe("ws");
    } finally {
      a.close();
      b.close();
    }
  });

  it("subscribe/unsubscribe at runtime get acks", async () => {
    const sock = new ChannelSocket({
      getToken: () => ch.createToken({ channels: ["room:a", "room:b"] }),
      channels: ["room:a"],
      webSocket: wsCtor,
    });
    try {
      await sock.connect();
      const acked = await sock.subscribe(["room:b"]);
      expect(acked).toContain("room:b");
      sock.unsubscribe(["room:a"]);
    } finally {
      sock.close();
    }
  });

  it("subscriber-only socket cannot publish", async () => {
    const sock = new ChannelSocket({
      getToken: () => ch.createToken({ channels: ["room:x"] }),
      webSocket: wsCtor,
    });
    try {
      await sock.connect();
      await expect(sock.publish("room:x", { nope: true })).rejects.toThrow();
    } finally {
      sock.close();
    }
  });
});
