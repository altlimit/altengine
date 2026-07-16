"""altengine — official Python SDK for https://www.altengine.net
(managed datastore, search, and realtime channels behind one API key).

    from altengine import AltEngine, f, facet

    ae = AltEngine(api_key="ae_...")          # production api.altengine.net
    # ae = AltEngine(dev=True, api_key="dev") # local `altengine dev`

    db = ae.datastore("myapp")
    keys = db.put("todos", [{"data": {"title": "ship SDK"}}])

    idx = ae.search("myapp").index("products")
    idx.put([{"fields": [f.text("title", "Blue Shoes"), f.number("price", 59)]}])

    ch = ae.channel("myapp")
    tok = ch.create_token(channels=["room:1"])

Async twin: ``AsyncAltEngine`` (same surface, awaitable). The managed
WebSocket subscriber is ``ChannelSocket`` (``pip install altengine[ws]``).
"""

from __future__ import annotations

from typing import Optional

import httpx

from ._http import DEFAULT_BASE_URL, DEV_BASE_URL, AsyncHttp, Http
from .channel import AsyncChannelClient, ChannelClient
from .datastore import AsyncDatastoreClient, DatastoreClient
from .errors import AltEngineError, AltEngineNetworkError
from .fields import f, facet
from .search import AsyncSearchClient, AsyncSearchIndex, SearchClient, SearchIndex
from .socket import ChannelSocket, ChannelSocketError

__version__ = "0.1.0"

__all__ = [
    "AltEngine",
    "AsyncAltEngine",
    "AltEngineError",
    "AltEngineNetworkError",
    "ChannelSocket",
    "ChannelSocketError",
    "DatastoreClient",
    "AsyncDatastoreClient",
    "SearchClient",
    "SearchIndex",
    "AsyncSearchClient",
    "AsyncSearchIndex",
    "ChannelClient",
    "AsyncChannelClient",
    "DEFAULT_BASE_URL",
    "DEV_BASE_URL",
    "f",
    "facet",
]


class AltEngine:
    """Synchronous entry point.

    - ``api_key`` — falls back to the ``ALTENGINE_API_KEY`` env var
    - ``dev=True`` — target the local emulator (``altengine dev``)
    - ``base_url`` — explicit origin; also settable via ``ALTENGINE_URL``
      (resolution: ``base_url`` → ``dev`` → ``ALTENGINE_URL`` → production)

    Retryable failures (429 with Retry-After, 502/503/504, network) are
    retried up to 3 times with jittered backoff — except transactions and
    publishes, which are never auto-retried.
    """

    def __init__(
        self,
        api_key: Optional[str] = None,
        base_url: Optional[str] = None,
        dev: bool = False,
        timeout: float = 30.0,
        max_attempts: int = 3,
        client: Optional[httpx.Client] = None,
    ) -> None:
        self._http = Http(
            api_key=api_key, base_url=base_url, dev=dev, timeout=timeout, max_attempts=max_attempts, client=client
        )

    def datastore(self, instance: str, namespace: str = "") -> DatastoreClient:
        """Client for a datastore instance (namespace-bound; default ``""``)."""
        return DatastoreClient(self._http, instance, namespace)

    def search(self, instance: str, namespace: str = "") -> SearchClient:
        """Client for a search instance (namespace-bound; default ``""``)."""
        return SearchClient(self._http, instance, namespace)

    def channel(self, instance: str) -> ChannelClient:
        """Server-side client for a channel instance (tokens, publish, presence)."""
        return ChannelClient(self._http, instance)

    def close(self) -> None:
        self._http.close()

    def __enter__(self) -> "AltEngine":
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()


class AsyncAltEngine:
    """Asynchronous entry point — same surface as :class:`AltEngine`, awaitable."""

    def __init__(
        self,
        api_key: Optional[str] = None,
        base_url: Optional[str] = None,
        dev: bool = False,
        timeout: float = 30.0,
        max_attempts: int = 3,
        client: Optional[httpx.AsyncClient] = None,
    ) -> None:
        self._http = AsyncHttp(
            api_key=api_key, base_url=base_url, dev=dev, timeout=timeout, max_attempts=max_attempts, client=client
        )

    def datastore(self, instance: str, namespace: str = "") -> AsyncDatastoreClient:
        return AsyncDatastoreClient(self._http, instance, namespace)

    def search(self, instance: str, namespace: str = "") -> AsyncSearchClient:
        return AsyncSearchClient(self._http, instance, namespace)

    def channel(self, instance: str) -> AsyncChannelClient:
        return AsyncChannelClient(self._http, instance)

    async def close(self) -> None:
        await self._http.close()

    async def __aenter__(self) -> "AsyncAltEngine":
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.close()
