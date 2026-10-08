import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { ChannelSocket } from "../../src/channel/socket.js";

describe("package exports", () => {
  // A CommonJS consumer was handed the ESM declaration file, which TypeScript (node16/bundler
  // resolution) reports as an ESM module masquerading as CJS. Each condition carries its own types.
  it("gives require() its own .d.cts types", () => {
    const pkg = JSON.parse(readFileSync(new URL("../../package.json", import.meta.url), "utf8"));
    for (const [sub, entry] of Object.entries<any>(pkg.exports)) {
      expect(entry.require?.types, sub).toMatch(/\.d\.cts$/);
      expect(entry.require?.default, sub).toMatch(/\.cjs$/);
      expect(entry.import?.types, sub).toMatch(/\.d\.ts$/);
      expect(entry.import?.default, sub).toMatch(/\.js$/);
    }
  });
});

describe("ChannelSocket without a global WebSocket (Node 20)", () => {
  // `WebSocket.OPEN` was read off the global, which Node 20 does not have — so with the `ws`
  // package passed in, checking an open socket threw a ReferenceError.
  it("works with only the webSocket option", async () => {
    const saved = (globalThis as any).WebSocket;
    delete (globalThis as any).WebSocket;
    try {
      const sent: string[] = [];
      class FakeWS {
        readyState = 0;
        onopen?: () => void;
        onmessage?: (ev: { data: string }) => void;
        onclose?: (ev: { code: number }) => void;
        onerror?: () => void;
        constructor(_url: string) {
          setTimeout(() => {
            this.readyState = 1;
            this.onopen?.();
          }, 0);
        }
        send(s: string) {
          sent.push(s);
        }
        close() {
          this.readyState = 3;
        }
        addEventListener() {}
      }
      const sock = new ChannelSocket({
        getToken: () => ({ token: "t", ws_url: "ws://127.0.0.1:1/v1/channel/c/subscribe" }),
        webSocket: FakeWS as any,
        pingIntervalMs: 1_000_000,
      });
      await sock.connect();
      await sock.connect(); // already open: must not throw or dial again
      sock.unsubscribe(["room"]); // reads the open state
      expect(sent.some((s) => s.includes("unsubscribe"))).toBe(true);
      sock.close();
    } finally {
      (globalThis as any).WebSocket = saved;
    }
  });
});
