"""Errors raised by the altengine SDK."""

from __future__ import annotations

from typing import Any, Optional


class AltEngineError(Exception):
    """A structured altengine API error: the ``{error: {code, message, details?}}``
    envelope plus transport context (HTTP status, Retry-After).

    ``code`` is machine-readable (NOT_FOUND, INVALID_ARGUMENT, UNAUTHENTICATED,
    PERMISSION_DENIED, RATE_LIMITED, QUERY_TOO_COMPLEX, INDEX_REQUIRED,
    DOCUMENT_TOO_LARGE, INDEX_FULL, ALREADY_EXISTS, PRECONDITION_FAILED,
    INTERNAL, ...). The set is open — new server codes must not break clients.
    """

    def __init__(
        self,
        code: str,
        message: str,
        status: int,
        details: Any = None,
        retry_after: Optional[float] = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.status = status
        self.details = details
        #: Seconds to wait before retrying, from the ``Retry-After`` header (429s).
        self.retry_after = retry_after

    @property
    def retryable(self) -> bool:
        """Whether the request can be safely retried after a backoff.

        429 always carries Retry-After; 502/503/504 are transient. 507
        INDEX_FULL is NOT retryable.
        """
        return self.status in (429, 502, 503, 504)

    def __repr__(self) -> str:  # pragma: no cover
        return f"AltEngineError(code={self.code!r}, status={self.status}, message={self.message!r})"


class AltEngineNetworkError(Exception):
    """Raised when the network layer fails before an HTTP response exists."""
