/** Where the signed-in session is kept between page loads. Implement this to use
 * cookies, IndexedDB, React Native's AsyncStorage (via a sync cache), or nothing
 * at all. The default picks `localStorage` in the browser and falls back to an
 * in-memory store everywhere else (Node, SSR, private-mode failures). */
export interface TokenStorage {
  get(key: string): string | null;
  set(key: string, value: string): void;
  remove(key: string): void;
}

/** A `TokenStorage` that forgets everything when the process/tab goes away. */
export function memoryStorage(): TokenStorage {
  const map = new Map<string, string>();
  return {
    get: (k) => map.get(k) ?? null,
    set: (k, v) => void map.set(k, v),
    remove: (k) => void map.delete(k),
  };
}

/** `localStorage` when it's present AND writable, else an in-memory store.
 * Safari private mode throws on `setItem`, so this probes with a real write. */
export function defaultStorage(): TokenStorage {
  try {
    const ls = globalThis.localStorage;
    if (!ls) return memoryStorage();
    const probe = "__altengine_probe__";
    ls.setItem(probe, "1");
    ls.removeItem(probe);
    return {
      get: (k) => ls.getItem(k),
      set: (k, v) => ls.setItem(k, v),
      remove: (k) => ls.removeItem(k),
    };
  } catch {
    return memoryStorage();
  }
}
