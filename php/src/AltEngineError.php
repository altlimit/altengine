<?php

declare(strict_types=1);

namespace AltEngine;

/**
 * A structured altengine API error: the {error:{code,message,details?}}
 * envelope plus transport context (HTTP status, Retry-After).
 *
 * $errorCode is machine-readable (NOT_FOUND, INVALID_ARGUMENT, UNAUTHENTICATED,
 * PERMISSION_DENIED, RATE_LIMITED, QUERY_TOO_COMPLEX, INDEX_REQUIRED,
 * DOCUMENT_TOO_LARGE, INDEX_FULL, ALREADY_EXISTS, PRECONDITION_FAILED,
 * INTERNAL, ...). The set is open — new server codes must not break clients.
 */
class AltEngineError extends \RuntimeException
{
    public function __construct(
        public readonly string $errorCode,
        string $message,
        public readonly int $status,
        public readonly mixed $details = null,
        /** Seconds to wait before retrying, from the Retry-After header (429s). */
        public readonly ?float $retryAfter = null,
    ) {
        parent::__construct($message, $status);
    }

    /**
     * Whether the request can be safely retried after a backoff. 429 always
     * carries Retry-After; 502/503/504 are transient. 507 INDEX_FULL is NOT
     * retryable.
     */
    public function retryable(): bool
    {
        return in_array($this->status, [429, 502, 503, 504], true);
    }
}
