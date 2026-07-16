import { Http, seg } from "../http.js";
import type { PresenceResponse, TokenRequest, TokenResponse } from "./types.js";

/** Server-side client for one channel instance (API-key auth): mint subscriber
 * tokens, publish over HTTP, and read presence. For the browser/edge WebSocket
 * subscriber, use `ChannelSocket` from `@altengine/sdk/channel`. */
export class ChannelClient {
  readonly instance: string;
  private readonly http: Http;
  private readonly base: string;

  constructor(http: Http, instance: string) {
    this.http = http;
    this.instance = instance;
    this.base = `/v1/channel/${seg(instance)}`;
  }

  /** Mint a subscriber token (JWT) for the given channels. */
  async createToken(req: TokenRequest): Promise<TokenResponse> {
    return this.http.request("POST", `${this.base}/tokens`, { body: req });
  }

  /** Publish a message (framed ≤32 KiB) to a channel; returns the number of
   * subscribers reached. NOT retried automatically (a retry double-delivers). */
  async publish(channel: string, data: unknown): Promise<{ delivered: number }> {
    return this.http.request("POST", `${this.base}/publish`, { body: { channel, data }, retry: false });
  }

  /** Presence roster for a channel (requires the instance's presence flag). */
  async presence(channel: string): Promise<PresenceResponse> {
    return this.http.request("GET", `${this.base}/presence`, { query: { channel } });
  }
}
