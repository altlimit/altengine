/** Error codes emitted by the altengine API. The union is open (`string`) so new
 * server codes never break compilation. */
export type ErrorCode =
  | "NOT_FOUND"
  | "INVALID_ARGUMENT"
  | "UNAUTHENTICATED"
  | "PERMISSION_DENIED"
  | "RATE_LIMITED"
  | "QUERY_TOO_COMPLEX"
  | "INDEX_REQUIRED"
  | "DOCUMENT_TOO_LARGE"
  | "INDEX_FULL"
  | "ALREADY_EXISTS"
  | "PRECONDITION_FAILED"
  | "INTERNAL"
  | (string & {});

/** A structured altengine API error: the `{error:{code,message,details?}}` envelope
 * plus transport context (HTTP status, Retry-After). */
export class AltEngineError extends Error {
  readonly code: ErrorCode;
  readonly status: number;
  readonly details?: unknown;
  /** Seconds to wait before retrying, from the `Retry-After` header (429s). */
  readonly retryAfter?: number;

  constructor(opts: { code: ErrorCode; message: string; status: number; details?: unknown; retryAfter?: number }) {
    super(opts.message);
    this.name = "AltEngineError";
    this.code = opts.code;
    this.status = opts.status;
    this.details = opts.details;
    this.retryAfter = opts.retryAfter;
  }

  /** Whether the request can be safely retried after a backoff. 429 always carries
   * Retry-After; 502/503/504 are transient. 507 INDEX_FULL is NOT retryable. */
  get retryable(): boolean {
    if (this.status === 429) return true;
    return this.status === 502 || this.status === 503 || this.status === 504;
  }
}

/** Thrown when the network/fetch layer fails before an HTTP response exists. */
export class AltEngineNetworkError extends Error {
  readonly cause?: unknown;
  constructor(message: string, cause?: unknown) {
    super(message);
    this.name = "AltEngineNetworkError";
    this.cause = cause;
  }
}
