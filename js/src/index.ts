/** @altengine/sdk — official JavaScript/TypeScript SDK for altengine.
 *
 * ```ts
 * import { AltEngine, f } from "@altengine/sdk";
 *
 * const ae = new AltEngine({ baseUrl: "http://127.0.0.1:9191", apiKey: "dev" });
 *
 * const db = ae.datastore("myapp");
 * const { keys } = await db.put("todos", [{ data: { title: "ship SDK" } }]);
 *
 * const idx = ae.search("myapp").index("products");
 * await idx.put([{ fields: [f.text("title", "Blue Shoes"), f.number("price", 59)] }]);
 *
 * const ch = ae.channel("myapp");
 * const { token, ws_url } = await ch.createToken({ channels: ["room:1"] });
 * ```
 *
 * For the browser WebSocket subscriber, import from `@altengine/sdk/channel`.
 */

import { Http, type ClientOptions } from "./http.js";
import { DatastoreClient, type DatastoreOptions } from "./datastore/client.js";
import { SearchClient, type SearchOptions } from "./search/client.js";
import { ChannelClient } from "./channel/client.js";

export class AltEngine {
  private readonly http: Http;

  constructor(opts: ClientOptions) {
    this.http = new Http(opts);
  }

  /** Client for a datastore instance (namespace-bound; default `""`). */
  datastore(instance: string, opts?: DatastoreOptions): DatastoreClient {
    return new DatastoreClient(this.http, instance, opts);
  }

  /** Client for a search instance (namespace-bound; default `""`). */
  search(instance: string, opts?: SearchOptions): SearchClient {
    return new SearchClient(this.http, instance, opts);
  }

  /** Server-side client for a channel instance (tokens, publish, presence). */
  channel(instance: string): ChannelClient {
    return new ChannelClient(this.http, instance);
  }
}

export { AltEngineError, AltEngineNetworkError, type ErrorCode } from "./errors.js";
export type { ClientOptions, RetryOptions, RequestOptions } from "./http.js";

export { DatastoreClient, NamespaceAdmin, type DatastoreOptions } from "./datastore/client.js";
export * from "./datastore/types.js";

export { SearchClient, SearchIndex, type SearchOptions } from "./search/client.js";
export * from "./search/types.js";
export { f, facet } from "./search/fields.js";

export { ChannelClient } from "./channel/client.js";
export * from "./channel/types.js";

// Also exported from the browser-safe `@altengine/sdk/channel` subpath.
export { ChannelSocket, type ChannelSocketOptions, type SocketState, type SocketToken } from "./channel/socket.js";
