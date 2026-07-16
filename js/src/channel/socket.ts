/** ChannelSocket — the browser/edge WebSocket subscriber for altengine channels.
 *
 * Standalone and browser-safe: no API keys, no other SDK imports. Your backend
 * mints a subscriber token (`ChannelClient.createToken`) and hands `{token, ws_url}`
 * to the page; the socket does the rest — connect, auto-reconnect with backoff,
 * token re-mint on reconnect, resubscribe, and keepalive pings.
 */

import type { ChannelMessage } from "./types.js";

export type SocketState = "idle" | "connecting" | "open" | "reconnecting" | "closed";

export interface SocketToken {
  token: string;
  /** Ready-made subscribe URL from the token response. Optional when `url` is set. */
  ws_url?: string;
}

export interface ChannelSocketOptions {
  /** Called before every connection attempt (including reconnects) so tokens are
   * always fresh. Return the token-mint response (or `{token}` with `url` set). */
  getToken: () => Promise<SocketToken> | SocketToken;
  /** Explicit subscribe URL (`wss://…/v1/channel/<instance>/subscribe`) when the
   * token response's `ws_url` isn't used; `?token=` is appended automatically. */
  url?: string;
  /** Subset of the token's channels to subscribe on connect (default: all). */
  channels?: string[];
  /** WebSocket constructor for runtimes without a global one. */
  webSocket?: new (url: string) => WebSocket;
  /** Keepalive ping interval in ms (default 30000). A silent connection past
   * ~1.5 intervals is treated as dead and reconnected. */
  pingIntervalMs?: number;
  /** Reconnect backoff base/cap in ms (defaults 250 / 30000). */
  backoffBaseMs?: number;
  backoffMaxMs?: number;
}

export interface SocketEvents {
  message: ChannelMessage;
  state: SocketState;
  error: Error;
}

type Listener<T> = (payload: T) => void;

export class ChannelSocket {
  private readonly opts: ChannelSocketOptions;
  private readonly listeners: { [K in keyof SocketEvents]: Set<Listener<SocketEvents[K]>> } = {
    message: new Set(),
    state: new Set(),
    error: new Set(),
  };

  private ws?: WebSocket;
  private stateValue: SocketState = "idle";
  private desired = new Set<string>();
  private explicitChannels: boolean;
  private closedByUser = false;
  private attempts = 0;
  private pingTimer?: ReturnType<typeof setInterval>;
  private lastActivity = 0;
  private reconnectTimer?: ReturnType<typeof setTimeout>;
  private pendingSubscribes: { channels: string[]; resolve: (channels: string[]) => void; reject: (e: Error) => void }[] = [];
  private pendingPublishes: { resolve: (delivered: number) => void; reject: (e: Error) => void }[] = [];

  constructor(opts: ChannelSocketOptions) {
    this.opts = opts;
    this.explicitChannels = opts.channels !== undefined;
    for (const c of opts.channels ?? []) this.desired.add(c);
  }

  get state(): SocketState {
    return this.stateValue;
  }

  on<K extends keyof SocketEvents>(event: K, fn: Listener<SocketEvents[K]>): () => void {
    this.listeners[event].add(fn);
    return () => this.listeners[event].delete(fn);
  }

  private emit<K extends keyof SocketEvents>(event: K, payload: SocketEvents[K]): void {
    for (const fn of this.listeners[event]) fn(payload);
  }

  private setState(s: SocketState): void {
    if (this.stateValue === s) return;
    this.stateValue = s;
    this.emit("state", s);
  }

  /** Connect (resolves once the socket is open). Reconnects automatically until
   * `close()` is called. */
  async connect(): Promise<void> {
    this.closedByUser = false;
    if (this.ws && (this.ws.readyState === WebSocket.OPEN || this.ws.readyState === WebSocket.CONNECTING)) return;
    await this.dial();
  }

  /** Stop reconnecting and close the connection. */
  close(): void {
    this.closedByUser = true;
    if (this.reconnectTimer) clearTimeout(this.reconnectTimer);
    this.stopPing();
    this.failPending(new Error("socket closed"));
    this.ws?.close(1000, "client close");
    this.ws = undefined;
    this.setState("closed");
  }

  /** Subscribe to more channels (tracked and re-applied after reconnects).
   * Resolves with the server's ack. */
  async subscribe(channels: string[]): Promise<string[]> {
    for (const c of channels) this.desired.add(c);
    this.explicitChannels = true;
    if (!this.isOpen()) return [...this.desired];
    return new Promise<string[]>((resolve, reject) => {
      this.pendingSubscribes.push({ channels, resolve, reject });
      this.send({ type: "subscribe", channels });
    });
  }

  /** Unsubscribe from channels. */
  unsubscribe(channels: string[]): void {
    for (const c of channels) this.desired.delete(c);
    this.explicitChannels = true;
    if (this.isOpen()) this.send({ type: "unsubscribe", channels });
  }

  /** Publish over the socket (requires a ws-capable `publish` token). Resolves
   * with the delivered count from the server's ack. */
  async publish(channel: string, data: unknown): Promise<number> {
    if (!this.isOpen()) throw new Error("socket is not open");
    return new Promise<number>((resolve, reject) => {
      this.pendingPublishes.push({ resolve, reject });
      this.send({ type: "publish", channel, data });
    });
  }

  // --- internals ---

  private isOpen(): boolean {
    return !!this.ws && this.ws.readyState === WebSocket.OPEN;
  }

  private send(frame: unknown): void {
    this.ws?.send(typeof frame === "string" ? frame : JSON.stringify(frame));
  }

  private buildUrl(tok: SocketToken): string {
    let raw = tok.ws_url ?? this.opts.url;
    if (!raw) throw new Error("no WebSocket URL: token response had no ws_url and no url option was set");
    const url = new URL(raw);
    if (!url.searchParams.has("token")) url.searchParams.set("token", tok.token);
    // Narrow the initial subscription to the explicitly requested subset.
    if (this.explicitChannels && this.desired.size) {
      url.searchParams.set("channels", [...this.desired].join(","));
    }
    return url.toString();
  }

  private async dial(): Promise<void> {
    this.setState(this.attempts === 0 ? "connecting" : "reconnecting");
    let tok: SocketToken;
    try {
      tok = await this.opts.getToken();
    } catch (err) {
      this.emit("error", err instanceof Error ? err : new Error(String(err)));
      this.scheduleReconnect();
      return;
    }

    const WS = this.opts.webSocket ?? (globalThis.WebSocket as typeof WebSocket | undefined);
    if (!WS) throw new Error("no WebSocket implementation; pass { webSocket } (e.g. `ws` on Node < 22)");

    await new Promise<void>((resolve) => {
      let settled = false;
      const ws = new WS(this.buildUrl(tok));
      this.ws = ws;

      ws.onopen = () => {
        settled = true;
        this.attempts = 0;
        this.lastActivity = Date.now();
        this.setState("open");
        this.startPing();
        resolve();
      };
      ws.onmessage = (ev: MessageEvent) => this.handleMessage(ev);
      ws.onerror = () => {
        // onclose always follows; error details aren't portable across runtimes.
      };
      ws.onclose = (ev: CloseEvent) => {
        this.stopPing();
        this.failPending(new Error(`socket closed (${ev.code})`));
        if (!settled) {
          settled = true;
          resolve();
        }
        if (!this.closedByUser) this.scheduleReconnect();
      };
    });
  }

  private handleMessage(ev: MessageEvent): void {
    this.lastActivity = Date.now();
    const raw = typeof ev.data === "string" ? ev.data : "";
    if (raw === "pong" || raw === "ping") return;
    let frame: any;
    try {
      frame = JSON.parse(raw);
    } catch {
      return;
    }
    switch (frame?.type) {
      case "subscribed": {
        // Only settle the oldest pending subscribe if this ack covers its channels —
        // an unsolicited ack (e.g. for the connect-time ?channels= subscription)
        // arriving late must not steal a runtime subscribe()'s resolution.
        const head = this.pendingSubscribes[0];
        const acked = new Set<string>(frame.channels ?? []);
        if (head && head.channels.every((c) => acked.has(c))) {
          this.pendingSubscribes.shift()!.resolve(frame.channels ?? []);
        }
        return;
      }
      case "published":
        this.pendingPublishes.shift()?.resolve(frame.delivered ?? 0);
        return;
      case "error": {
        const err = new Error(typeof frame.error === "string" ? frame.error : JSON.stringify(frame.error));
        // An error ack settles the oldest pending request, if any.
        (this.pendingPublishes.shift() ?? this.pendingSubscribes.shift())?.reject(err);
        this.emit("error", err);
        return;
      }
      default:
        if (frame && typeof frame.channel === "string" && frame.type === undefined) {
          this.emit("message", frame as ChannelMessage);
        }
    }
  }

  private startPing(): void {
    const interval = this.opts.pingIntervalMs ?? 30_000;
    this.stopPing();
    this.pingTimer = setInterval(() => {
      if (!this.isOpen()) return;
      if (Date.now() - this.lastActivity > interval * 1.5) {
        // Dead connection: no traffic (not even pongs). Force a reconnect.
        this.ws?.close(4000, "keepalive timeout");
        return;
      }
      this.send("ping");
    }, interval);
  }

  private stopPing(): void {
    if (this.pingTimer) clearInterval(this.pingTimer);
    this.pingTimer = undefined;
  }

  private scheduleReconnect(): void {
    if (this.closedByUser) return;
    const base = this.opts.backoffBaseMs ?? 250;
    const cap = this.opts.backoffMaxMs ?? 30_000;
    const delay = Math.min(base * 2 ** this.attempts, cap) * (0.5 + Math.random() * 0.5);
    this.attempts++;
    this.setState("reconnecting");
    this.reconnectTimer = setTimeout(() => void this.dial(), delay);
  }

  private failPending(err: Error): void {
    for (const p of this.pendingSubscribes.splice(0)) p.reject(err);
    for (const p of this.pendingPublishes.splice(0)) p.reject(err);
  }
}

export type { ChannelMessage } from "./types.js";
