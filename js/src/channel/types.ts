/** Wire types for the channel (realtime pub/sub) data plane. */

export type PublishMode = boolean | "http" | "ws" | "all";

export interface TokenRequest {
  /** Channels this token may subscribe to (≤100 names, each ≤200 bytes). */
  channels: string[];
  /** Default 3600, max 14400. */
  ttl_seconds?: number;
  /** Grant publish capability: `"http"`, `"ws"`, `"all"` (or `true` ≡ all). Minting
   * a publish-capable token requires a `write` grant on the API key. */
  publish?: PublishMode;
  /** Stable presence identity (≤128 bytes) bound into the token server-side. */
  presence_id?: string;
}

export interface TokenResponse {
  /** Subscriber JWT — safe to hand to a browser. */
  token: string;
  /** Unix seconds. */
  expires_at: number;
  channels: string[];
  publish: PublishMode;
  /** Ready-made WebSocket URL (`wss://…/subscribe?token=…`). */
  ws_url: string;
  presence_id?: string;
}

/** A delivered message frame (same shape over WS and HTTP publish). */
export interface ChannelMessage<T = unknown> {
  channel: string;
  data: T;
  /** Server timestamp (unix ms). */
  ts: number;
}

export interface PresenceMember {
  id: string;
  connections: number;
}

export interface PresenceResponse {
  channel: string;
  occupancy: number;
  member_count: number;
  /** At most 1000 members; see `truncated`. */
  members: PresenceMember[];
  truncated: boolean;
}
