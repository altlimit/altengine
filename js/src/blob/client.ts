import { Http, seg, type ClientOptions } from "../http.js";
import { AltEngineError } from "../errors.js";
import type {
  BlobDownload,
  BlobListQuery,
  BlobPage,
  BlobRecord,
  MintedUpload,
  MultipartBegin,
  MultipartUrls,
  UploadOptions,
  UploadUrlRequest,
} from "./types.js";

/**
 * What one presigned PUT can carry, and therefore where multipart starts.
 *
 * Not our number: object storage caps a single upload here, and a presigned URL is exactly one
 * request. The instance's own size limit is a cost control and a different question.
 */
export const MAX_SINGLE_PUT_BYTES = 5 * 1024 * 1024 * 1024;

const DEFAULT_CONTENT_TYPE = "application/octet-stream";

/**
 * Client for one blob instance.
 *
 * BYTES NEVER TRAVEL THROUGH THE API. Every upload and download here goes to a presigned URL,
 * so the API sees a reservation and storage sees the file. There is no commit step: the row is
 * promoted by the side that received the bytes.
 *
 * Levels are a ceiling — `read < write < full`. Uploading needs `write`; `delete` needs `full`.
 *
 * A CONTAINER JOB'S TOKEN IS NARROWER THAN AN API KEY, and the two refusals it carries are
 * refusals, not omissions: it may not `list` a store, and it may not publish — `setPublic`, and
 * `public: true` on an upload. Both come back as 403s naming what was refused. A job is told
 * its inputs; it does not go looking, and publishing is the one act that outlives the credential
 * that made it.
 */
export class BlobClient {
  readonly instance: string;
  private readonly http: Http;
  private readonly base: string;

  constructor(http: Http, instance: string) {
    this.http = http;
    this.instance = instance;
    this.base = `/v1/blob/${seg(instance)}`;
  }

  // --- uploading ------------------------------------------------------------

  /**
   * Store a file. Returns the finished object.
   *
   * Picks the transfer for you: one presigned PUT, or a multipart upload when a `Blob`/`File` is
   * larger than storage will take in a single request. `content` may be a string, an
   * `ArrayBuffer`, a typed array, or a `Blob`/`File` — only a `Blob` can be uploaded in parts,
   * since parts are sliced from it rather than held in memory.
   *
   * `contentType` is worth passing: without one an object is stored as
   * `application/octet-stream`, which a browser downloads rather than renders. A `File` carries
   * its own.
   */
  async put(
    name: string,
    content: string | ArrayBuffer | ArrayBufferView | Blob,
    opts: UploadOptions = {}
  ): Promise<BlobRecord> {
    const body = toBody(content);
    const contentType = opts.contentType ?? blobType(content) ?? DEFAULT_CONTENT_TYPE;
    if (isBlob(body) && body.size > MAX_SINGLE_PUT_BYTES) {
      return this.putMultipart(name, body, contentType, opts);
    }
    const size = isBlob(body) ? body.size : body.byteLength;
    const minted = await this.uploadUrl({
      name,
      size,
      content_type: contentType,
      public: opts.public,
      meta: opts.meta,
    });
    await this.http.transfer(minted.upload_url, {
      method: minted.method,
      // content-length is set by the runtime from the body. It is signed into the URL, so a
      // body of a different length is refused by storage rather than stored short.
      headers: { "content-type": contentType },
      body: body as BodyInit,
      signal: opts.signal,
    });
    opts.onProgress?.({ uploaded: size, total: size });
    return this.read(minted.id);
  }

  /**
   * Mint a URL a client PUTs bytes to directly, without handing that client an API key.
   *
   * This is how a browser uploads: your backend decides whether this user may, and how big the
   * file may be, then hands over a URL that can do exactly that one thing for fifteen minutes.
   * The size is signed in, so it is the real bound on what the holder can make you store.
   */
  async uploadUrl(req: UploadUrlRequest): Promise<MintedUpload> {
    return this.http.request("POST", `${this.base}/uploads`, { body: req, retry: false });
  }

  /** Reserve an object and sign the call that opens a multipart upload. Use `put` unless you
   *  are driving the transfer yourself. */
  async beginMultipart(req: UploadUrlRequest): Promise<MultipartBegin> {
    return this.http.request("POST", `${this.base}/uploads/multipart`, { body: req, retry: false });
  }

  /** Sign a window of part URLs, plus the complete and abort URLs. At most 100 parts per call. */
  async multipartUrls(req: { id: string; upload_id: string; from?: number; count?: number }): Promise<MultipartUrls> {
    return this.http.request("POST", `${this.base}/uploads/multipart/urls`, { body: req, retry: false });
  }

  // --- reading --------------------------------------------------------------

  /** An object's metadata plus a short-lived download URL. */
  async get(id: string): Promise<BlobDownload> {
    return this.http.request("GET", `${this.base}/${seg(id)}`);
  }

  /** An object's bytes, fetched from storage rather than through the API. */
  async bytes(id: string, opts: { signal?: AbortSignal } = {}): Promise<Uint8Array> {
    const { download_url } = await this.get(id);
    const res = await this.http.transfer(download_url, { signal: opts.signal });
    return new Uint8Array(await res.arrayBuffer());
  }

  /**
   * One page of objects, newest first.
   *
   * PAGED, and the cursor matters: a store is small in development and unbounded in a real
   * account, so a caller that ignores it is looking at part of one.
   *
   * Refused for a container job token — a job is told its inputs rather than enumerating the
   * store it writes to.
   */
  async list(query: BlobListQuery = {}): Promise<BlobPage> {
    return this.http.request("GET", this.base, { query });
  }

  /** Every object, walking the pages for you. */
  async *listAll(query: Omit<BlobListQuery, "cursor"> = {}): AsyncGenerator<BlobRecord> {
    let cursor: string | undefined;
    for (;;) {
      const page = await this.list({ ...query, cursor });
      for (const b of page.blobs) yield b;
      if (!page.cursor) return;
      cursor = page.cursor;
    }
  }

  // --- changing -------------------------------------------------------------

  /**
   * Publish or unpublish an object. A public object is served from a stable URL with no
   * credential at all; unpublishing drops the cached copies too.
   *
   * Refused for a container job token: publishing is the one operation whose effect outlives the
   * credential that made it.
   */
  async setPublic(id: string, isPublic = true): Promise<BlobRecord> {
    const { blob } = await this.http.request<{ blob: BlobRecord }>("POST", `${this.base}/${seg(id)}/public`, {
      body: { public: isPublic },
      retry: false,
    });
    return blob;
  }

  /** Delete up to 200 objects. Needs `full`; ids that do not exist are not counted. */
  async delete(ids: string[]): Promise<{ deleted: number }> {
    return this.http.request("POST", `${this.base}/delete`, { body: { ids }, retry: false });
  }

  // --- the multipart transfer -----------------------------------------------

  /**
   * Cut a large file up, send the parts, and let storage glue them back together — so what the
   * customer downloads is one ordinary file rather than a split archive they need a tool for.
   *
   * Parts go up one at a time: these bytes usually leave an office connection, and a part that
   * fails is a part re-sent in full. ON ANY FAILURE THE UPLOAD IS ABORTED, because parts of an
   * upload nobody completed keep billing and no listing shows them.
   */
  private async putMultipart(
    name: string,
    file: Blob,
    contentType: string,
    opts: UploadOptions
  ): Promise<BlobRecord> {
    const begun = await this.beginMultipart({
      name,
      size: file.size,
      content_type: contentType,
      public: opts.public,
      meta: opts.meta,
    });

    // The content type is pinned by the call that creates the upload — storage takes the
    // finished object's metadata from it — and it is signed into that URL.
    const created = await this.http.transfer(begun.create_url, {
      method: "POST",
      headers: { "content-type": contentType },
      signal: opts.signal,
    });
    const uploadId = /<UploadId>([^<]+)<\/UploadId>/.exec(await created.text())?.[1];
    if (!uploadId) throw new AltEngineError({ code: "INTERNAL", message: "storage did not name an upload id", status: 502 });

    let abortUrl: string | undefined;
    try {
      const etags: { part_number: number; etag: string }[] = [];
      let uploaded = 0;
      let next = 1;
      let completeUrl = "";
      while (next <= begun.parts) {
        const signed = await this.multipartUrls({ id: begun.id, upload_id: uploadId, from: next, count: begun.parts - next + 1 });
        abortUrl = signed.abort_url;
        completeUrl = signed.complete_url;
        // A window that signs nothing would spin here forever, which reads as a hang rather
        // than a failure.
        if (!signed.part_urls.length || signed.part_urls[0]!.part_number !== next) {
          throw new AltEngineError({
            code: "INTERNAL",
            message: `asked for part ${next} and was signed none`,
            status: 502,
          });
        }
        for (const part of signed.part_urls) {
          if (part.part_number > begun.parts) break;
          const start = (part.part_number - 1) * begun.part_size;
          const chunk = file.slice(start, Math.min(start + begun.part_size, file.size));
          const res = await this.http.transfer(part.url, { method: "PUT", body: chunk, signal: opts.signal });
          const etag = res.headers.get("etag");
          // Without it the completion cannot name the part. In a browser this is the CORS
          // configuration talking, not the upload: ETag has to be an exposed header.
          if (!etag) {
            throw new AltEngineError({
              code: "INTERNAL",
              message: `storage returned no ETag for part ${part.part_number}; it must be an exposed response header`,
              status: 502,
            });
          }
          etags.push({ part_number: part.part_number, etag });
          uploaded += chunk.size;
          opts.onProgress?.({ uploaded, total: file.size });
          next = part.part_number + 1;
        }
      }

      await this.http.transfer(completeUrl, {
        method: "POST",
        headers: { "content-type": "application/xml" },
        body: completeXml(etags),
        signal: opts.signal,
      });
    } catch (err) {
      // An abort URL only exists once a window has been signed, so a failure before the first
      // one has to go and ask for it — otherwise an upload that failed early is an upload
      // nothing ever aborts.
      const url = abortUrl ?? (await this.multipartUrls({ id: begun.id, upload_id: uploadId, from: 1, count: 1 })
        .then((u) => u.abort_url)
        .catch(() => undefined));
      // Best effort, and it must not replace the real error: the caller needs to know why the
      // upload failed, not why the tidy-up did.
      if (url) await this.http.transfer(url, { method: "DELETE" }).catch(() => undefined);
      throw err;
    }
    return this.read(begun.id);
  }

  /** The finished record. A read, so it needs no grant an upload did not already have —
   *  `write` outranks `read`. */
  private async read(id: string): Promise<BlobRecord> {
    return (await this.get(id)).blob;
  }
}

/**
 * The blob client for the store a container job was handed, built from the two variables the
 * platform puts in every job an instance with a `blobStore` launches: `AE_BLOB_URL` (the store's
 * own endpoint) and `AE_BLOB_TOKEN` (a bearer that expires with the job).
 *
 * That token is scoped to one store and one job, at `write`. So it puts and gets; it does not
 * delete, does not `list` the store, and does not publish. Each of those comes back as a 403
 * naming what was refused.
 */
export function blobFromJobEnv(opts: Omit<ClientOptions, "baseUrl" | "apiKey" | "auth"> = {}): BlobClient {
  const url = typeof process !== "undefined" ? process.env?.AE_BLOB_URL : undefined;
  const token = typeof process !== "undefined" ? process.env?.AE_BLOB_TOKEN : undefined;
  if (!url || !token) {
    throw new AltEngineError({
      code: "FAILED_PRECONDITION",
      message:
        "AE_BLOB_URL and AE_BLOB_TOKEN are not set — this is not a container job, or its instance names no blobStore",
      status: 400,
    });
  }
  const m = /^(.*)\/v1\/blob\/([^/]+)\/*$/.exec(url);
  if (!m) {
    throw new AltEngineError({ code: "INVALID_ARGUMENT", message: `AE_BLOB_URL is not a blob endpoint: ${url}`, status: 400 });
  }
  return new BlobClient(new Http({ ...opts, baseUrl: m[1]!, apiKey: token }), decodeURIComponent(m[2]!));
}

/** The completion document: which parts, in which order, and the digest storage gave each. */
function completeXml(parts: { part_number: number; etag: string }[]): string {
  const body = [...parts]
    .sort((a, b) => a.part_number - b.part_number)
    // The ETag travels verbatim, quotes included, because that is the form storage compares
    // against. Anything that could break the document out of its element is dropped.
    .map((p) => `<Part><PartNumber>${p.part_number}</PartNumber><ETag>${p.etag.replace(/[<>&]/g, "")}</ETag></Part>`)
    .join("");
  return `<CompleteMultipartUpload>${body}</CompleteMultipartUpload>`;
}

/** Structural, not `instanceof`: a `File` from one realm is not a `Blob` from another, and
 *  Node has had two `Blob` constructors at once. `size` plus `slice` is what this needs. */
const isBlob = (v: unknown): v is Blob =>
  typeof v === "object" &&
  v !== null &&
  typeof (v as Blob).size === "number" &&
  typeof (v as Blob).slice === "function";

const blobType = (content: unknown): string | undefined =>
  isBlob(content) && content.type ? content.type : undefined;

/** Whatever the caller passed, as something with a known byte length. */
function toBody(content: string | ArrayBuffer | ArrayBufferView | Blob): Blob | Uint8Array {
  if (typeof content === "string") return new TextEncoder().encode(content);
  if (isBlob(content)) return content;
  if (content instanceof ArrayBuffer) return new Uint8Array(content);
  if (ArrayBuffer.isView(content)) return new Uint8Array(content.buffer, content.byteOffset, content.byteLength);
  throw new AltEngineError({
    code: "INVALID_ARGUMENT",
    message: "content must be a string, an ArrayBuffer, a typed array, or a Blob",
    status: 400,
  });
}
