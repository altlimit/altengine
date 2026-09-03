/** Sleep, abortable — so a `wait` inside a request handler dies with the request rather than
 *  holding it open until work nobody is watching any more finishes. */
export function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(signal.reason ?? new Error("aborted"));
    const t = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    function onAbort() {
      clearTimeout(t);
      reject(signal?.reason ?? new Error("aborted"));
    }
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}
