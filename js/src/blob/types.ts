/** Wire types for the blob data plane — files, stored and served. */

/** One stored object. */
export interface BlobRecord {
  id: string;
  /** Display name. Cosmetic — the id is the lookup key — and it is the segment that appears
   *  in a public URL. */
  name: string;
  size: number;
  content_type: string;
  etag: string;
  /** `pending` is a reservation whose bytes have not landed. A read promotes it once they
   *  have, so an object you just uploaded reads as `ready`. */
  status: "pending" | "ready";
  public: boolean;
  created: number;
  updated: number;
  meta: Record<string, unknown>;
  /** `blob:<instance>:<object>` — the portable handle to store elsewhere. It is an identifier,
   *  not a capability: holding one lets nobody read anything. */
  blobkey: string;
  /** The stable public URL, or null when the object is private. */
  url: string | null;
}

export interface UploadUrlRequest {
  name?: string;
  /** The exact byte length you will upload. Required, and signed into the URL — sending a
   *  different number of bytes is refused by storage. */
  size: number;
  /** Defaults to `application/octet-stream`. Also signed in. */
  content_type?: string;
  /** Omit to take the instance's default. A container job token is refused `true`. */
  public?: boolean;
  meta?: Record<string, unknown>;
}

/** A reservation and the URL its bytes go to. */
export interface MintedUpload {
  id: string;
  blobkey: string;
  upload_url: string;
  expires_at: number;
  /** What the PUT must carry, byte for byte. `content-length` is set by the runtime from the
   *  body you send. */
  required_headers: Record<string, string>;
  method: "PUT";
}

/** The first half of a multipart upload: POST `create_url` and storage answers with XML
 *  naming the upload id. */
export interface MultipartBegin {
  id: string;
  blobkey: string;
  key: string;
  part_size: number;
  parts: number;
  create_url: string;
  expires_at: number;
}

/** A window of part URLs, plus the two that end the upload either way. */
export interface MultipartUrls {
  part_urls: { part_number: number; url: string }[];
  complete_url: string;
  /** DELETE here on any failure. Parts of an upload nobody completed still bill. */
  abort_url: string;
  expires_at: number;
}

export interface BlobDownload {
  blob: BlobRecord;
  /** A signed link, minted for this response and good for a few minutes. Fetch what you need
   *  rather than storing the URL. */
  download_url: string;
  expires_at: number;
}

export interface BlobPage {
  blobs: BlobRecord[];
  cursor: string | null;
}

/** A type alias rather than an interface so it satisfies the query-string record. */
export type BlobListQuery = {
  /** Matches the start of the object's name. */
  prefix?: string;
  limit?: number;
  cursor?: string;
};

export interface UploadOptions {
  /** Defaults to a `Blob`/`File`'s own `type`, then to `application/octet-stream`. */
  contentType?: string;
  /** Omit to take the instance's default. */
  public?: boolean;
  meta?: Record<string, unknown>;
  /** Aborts the transfer, and the multipart upload with it. */
  signal?: AbortSignal;
  /** Called after each part of a multipart upload. */
  onProgress?: (p: { uploaded: number; total: number }) => void;
}
