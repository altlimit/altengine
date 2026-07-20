/** Side-effect-free constants and path helpers shared by every client.
 *
 * These live apart from `http.ts` on purpose: the browser-safe entry points
 * (`@altengine/sdk/auth`, `@altengine/sdk/channel`) need the base URLs and path
 * encoders but must NOT pull in the API-key transport, which is a backend
 * credential path that has no business in a browser bundle. */

/** Production API origin used when no override is given. */
export const DEFAULT_BASE_URL = "https://api.altengine.net";
/** Where `altengine dev` (the local emulator) listens by default. */
export const DEV_BASE_URL = "http://127.0.0.1:9191";

/** Encode one path segment (instance/index/collection/key names). */
export const seg = (s: string | number): string => encodeURIComponent(String(s));

/** Encode a namespace path segment. The empty (default) namespace can't ride a
 * URL path — routers collapse the resulting "//", so it travels as the reserved
 * sentinel `_default` (the server maps it back to ""). */
export const nsSeg = (namespace: string): string => seg(namespace === "" ? "_default" : namespace);
