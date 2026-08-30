/** @altengine/sdk — official JavaScript/TypeScript SDK for altengine
 * (https://www.altengine.net).
 *
 * ```ts
 * import { AltEngine, f } from "@altengine/sdk";
 *
 * const ae = new AltEngine({ apiKey: "ae_..." });      // production api.altengine.net
 * // const ae = new AltEngine({ dev: true, apiKey: "dev" }); // local `altengine dev`
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
 * For client-side sign-in with no backend of your own, see `@altengine/sdk/auth`:
 *
 * ```ts
 * import { AuthClient } from "@altengine/sdk/auth";
 *
 * const auth = new AuthClient({ instance: "myapp-auth" });
 * await auth.signIn("alice@example.com", "hunter2");
 * const ae = new AltEngine({ auth });   // requests now carry the user's token
 * ```
 */

import { Http, type ClientOptions } from "./http.js";
import { DatastoreClient, type DatastoreOptions } from "./datastore/client.js";
import { SearchClient, type SearchOptions } from "./search/client.js";
import { ChannelClient } from "./channel/client.js";
import { AuthClient } from "./auth/client.js";
import { AutomationClient } from "./automation/client.js";

export class AltEngine {
  private readonly http: Http;

  constructor(opts: ClientOptions = {}) {
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

  /** Server-side client for an automation instance — start runs on your own machines, read
   * what they produced, and declare schedules. Organization API key only: a run spends time on
   * hardware you own, so end-user identity tokens are refused as they are for containers. */
  automation(instance: string): AutomationClient {
    return new AutomationClient(this.http, instance);
  }

  /** Client for an auth instance — end-user sign-up/sign-in and identity tokens.
   * Shares this client's API origin but never its API key (auth endpoints are
   * public; the end user's own credentials are the trust boundary). For browser
   * apps, import `AuthClient` from `@altengine/sdk/auth` directly instead. */
  auth(instance: string): AuthClient {
    return new AuthClient({ instance, baseUrl: this.http.baseUrl });
  }
}

export { AltEngineError, AltEngineNetworkError, type ErrorCode } from "./errors.js";
export {
  DEFAULT_BASE_URL,
  DEV_BASE_URL,
  type ClientOptions,
  type RetryOptions,
  type RequestOptions,
  type TokenProvider,
} from "./http.js";

export { AuthClient, type AuthClientOptions } from "./auth/client.js";
export { memoryStorage, defaultStorage, type TokenStorage } from "./auth/storage.js";
export * from "./auth/types.js";

export { DatastoreClient, NamespaceAdmin, type DatastoreOptions } from "./datastore/client.js";
export * from "./datastore/types.js";

export { SearchClient, SearchIndex, type SearchOptions } from "./search/client.js";
export * from "./search/types.js";
export { f, facet } from "./search/fields.js";

export { AutomationClient } from "./automation/client.js";
export * from "./automation/types.js";

export { ChannelClient } from "./channel/client.js";
export * from "./channel/types.js";

// Also exported from the browser-safe `@altengine/sdk/channel` subpath.
export { ChannelSocket, type ChannelSocketOptions, type SocketState, type SocketToken } from "./channel/socket.js";
