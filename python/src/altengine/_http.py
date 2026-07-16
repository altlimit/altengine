"""Minimal JSON-over-HTTP transports (sync + async) shared by all service clients."""

from __future__ import annotations

import os
import random
import time
from typing import Any, Dict, Mapping, Optional

import httpx

from .errors import AltEngineError, AltEngineNetworkError

#: Production API origin used when no override is given.
DEFAULT_BASE_URL = "https://api.altengine.net"
#: Where ``altengine dev`` (the local emulator) listens by default.
DEV_BASE_URL = "http://127.0.0.1:9191"

_DEFAULT_MAX_ATTEMPTS = 3
_DEFAULT_BASE_DELAY = 0.25
_DEFAULT_MAX_DELAY = 4.0


def resolve_base_url(base_url: Optional[str], dev: bool) -> str:
    """Resolution order: explicit ``base_url`` → ``dev`` → ``ALTENGINE_URL`` env
    var → production."""
    if base_url:
        return base_url.rstrip("/")
    if dev:
        return DEV_BASE_URL
    return (os.environ.get("ALTENGINE_URL") or DEFAULT_BASE_URL).rstrip("/")


def _clean_query(query: Optional[Mapping[str, Any]]) -> Optional[Dict[str, str]]:
    if not query:
        return None
    out = {k: str(v).lower() if isinstance(v, bool) else str(v) for k, v in query.items() if v is not None}
    return out or None


def _to_api_error(res: httpx.Response) -> AltEngineError:
    code, message, details = "INTERNAL", f"HTTP {res.status_code}", None
    try:
        parsed = res.json()
        err = parsed.get("error") if isinstance(parsed, dict) else None
        if isinstance(err, dict):
            code = err.get("code") or code
            message = err.get("message") or message
            details = err.get("details")
    except Exception:
        pass  # non-JSON error body; keep the status-derived defaults
    retry_after = None
    ra = res.headers.get("retry-after")
    if ra is not None:
        try:
            retry_after = float(ra)
        except ValueError:
            pass
    return AltEngineError(code, message, res.status_code, details, retry_after)


def _backoff(attempt: int, err: Optional[AltEngineError]) -> float:
    delay = min(_DEFAULT_BASE_DELAY * 2 ** (attempt - 1), _DEFAULT_MAX_DELAY)
    if err is not None and err.retry_after is not None:
        delay = max(delay, err.retry_after)
    # Full jitter keeps concurrent retries from stampeding in unison.
    return delay * (0.5 + random.random() * 0.5)


class Http:
    """Synchronous transport over ``httpx.Client``."""

    def __init__(
        self,
        api_key: Optional[str] = None,
        base_url: Optional[str] = None,
        dev: bool = False,
        timeout: float = 30.0,
        max_attempts: int = _DEFAULT_MAX_ATTEMPTS,
        client: Optional[httpx.Client] = None,
    ) -> None:
        self.base_url = resolve_base_url(base_url, dev)
        self.api_key = api_key if api_key is not None else os.environ.get("ALTENGINE_API_KEY")
        self.max_attempts = max_attempts
        self._client = client or httpx.Client(timeout=timeout)

    def close(self) -> None:
        self._client.close()

    def request(
        self,
        method: str,
        path: str,
        *,
        query: Optional[Mapping[str, Any]] = None,
        body: Any = None,
        headers: Optional[Mapping[str, str]] = None,
        retry: bool = True,
    ) -> Any:
        attempts = self.max_attempts if retry else 1
        attempt = 0
        while True:
            attempt += 1
            try:
                return self._once(method, path, query, body, headers)
            except AltEngineError as err:
                if not err.retryable or attempt >= attempts:
                    raise
                time.sleep(_backoff(attempt, err))
            except AltEngineNetworkError:
                if attempt >= attempts:
                    raise
                time.sleep(_backoff(attempt, None))

    def _once(self, method, path, query, body, headers) -> Any:
        hdrs = dict(headers or {})
        if self.api_key:
            hdrs["authorization"] = f"Bearer {self.api_key}"
        try:
            res = self._client.request(
                method,
                self.base_url + path,
                params=_clean_query(query),
                json=body,
                headers=hdrs,
            )
        except httpx.HTTPError as exc:
            raise AltEngineNetworkError(f"request failed: {method} {path}: {exc}") from exc
        if res.status_code < 200 or res.status_code > 299:
            raise _to_api_error(res)
        if res.status_code == 204 or not res.content:
            return None
        return res.json()


class AsyncHttp:
    """Asynchronous transport over ``httpx.AsyncClient``."""

    def __init__(
        self,
        api_key: Optional[str] = None,
        base_url: Optional[str] = None,
        dev: bool = False,
        timeout: float = 30.0,
        max_attempts: int = _DEFAULT_MAX_ATTEMPTS,
        client: Optional[httpx.AsyncClient] = None,
    ) -> None:
        self.base_url = resolve_base_url(base_url, dev)
        self.api_key = api_key if api_key is not None else os.environ.get("ALTENGINE_API_KEY")
        self.max_attempts = max_attempts
        self._client = client or httpx.AsyncClient(timeout=timeout)

    async def close(self) -> None:
        await self._client.aclose()

    async def request(
        self,
        method: str,
        path: str,
        *,
        query: Optional[Mapping[str, Any]] = None,
        body: Any = None,
        headers: Optional[Mapping[str, str]] = None,
        retry: bool = True,
    ) -> Any:
        import asyncio

        attempts = self.max_attempts if retry else 1
        attempt = 0
        while True:
            attempt += 1
            try:
                return await self._once(method, path, query, body, headers)
            except AltEngineError as err:
                if not err.retryable or attempt >= attempts:
                    raise
                await asyncio.sleep(_backoff(attempt, err))
            except AltEngineNetworkError:
                if attempt >= attempts:
                    raise
                await asyncio.sleep(_backoff(attempt, None))

    async def _once(self, method, path, query, body, headers) -> Any:
        hdrs = dict(headers or {})
        if self.api_key:
            hdrs["authorization"] = f"Bearer {self.api_key}"
        try:
            res = await self._client.request(
                method,
                self.base_url + path,
                params=_clean_query(query),
                json=body,
                headers=hdrs,
            )
        except httpx.HTTPError as exc:
            raise AltEngineNetworkError(f"request failed: {method} {path}: {exc}") from exc
        if res.status_code < 200 or res.status_code > 299:
            raise _to_api_error(res)
        if res.status_code == 204 or not res.content:
            return None
        return res.json()


def seg(value: Any) -> str:
    """Encode one path segment (instance/index/collection/key names)."""
    from urllib.parse import quote

    return quote(str(value), safe="")
