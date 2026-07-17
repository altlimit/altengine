"""Datastore clients (sync + async). All payloads are wire-verbatim dicts —
what the REST API documents is exactly what you pass and get back."""

from __future__ import annotations

from typing import Any, AsyncIterator, Dict, Iterator, List, Optional, Sequence, Union

from ._http import AsyncHttp, Http, seg

Key = Union[str, int]
Document = Dict[str, Any]


def _key_str(key: Key) -> str:
    # Numeric keys are stored as decimal strings (5 ≡ "5").
    return str(key)


class DatastoreClient:
    """Client for one datastore instance, bound to a namespace (default ``""``)."""

    def __init__(self, http: Http, instance: str, namespace: str = "") -> None:
        self._http = http
        self.instance = instance
        self.namespace = namespace
        self._base = f"/v1/datastore/{seg(instance)}"
        self._ns = f"{self._base}/namespaces/{seg(namespace)}"

    def with_namespace(self, namespace: str) -> "DatastoreClient":
        """Same instance, different namespace."""
        return DatastoreClient(self._http, self.instance, namespace)

    def put(self, collection: str, documents: Sequence[Document]) -> List[str]:
        """Upsert up to 500 documents (``{"key": ..., "data": {...}}``; omit
        ``key`` for auto-id). Returns their keys in order."""
        res = self._http.request(
            "POST", f"{self._ns}/collections/{seg(collection)}/documents", body={"documents": list(documents)}
        )
        return res["keys"]

    def get(self, collection: str, key: Union[Key, Sequence[Key]]) -> Any:
        """Fetch one document (``None`` when missing), or — passed a list — up
        to 500 documents in one round trip, order-preserving with ``None``
        placeholders for missing keys (App Engine ``db.get`` semantics). Both
        forms ride the batch endpoint — there is no single-document route on
        the wire."""
        single = not isinstance(key, (list, tuple))
        keys = [key] if single else list(key)
        res = self._http.request(
            "POST", f"{self._ns}/collections/{seg(collection)}/documents/get", body={"keys": keys}
        )
        by_key = {d["key"]: d for d in res["documents"]}
        docs = [by_key.get(_key_str(k)) for k in keys]
        return docs[0] if single else docs

    def delete(self, collection: str, keys: Sequence[Key]) -> int:
        """Delete up to 500 documents by key; missing keys are no-ops. Returns
        the number actually deleted."""
        res = self._http.request(
            "POST", f"{self._ns}/collections/{seg(collection)}/documents/delete", body={"keys": list(keys)}
        )
        return res["deleted"]

    def query(self, collection: str, **req: Any) -> Dict[str, Any]:
        """Run one page of an index-served query. Keyword args are the wire
        request: ``where``, ``order``, ``limit`` (default 25, max 500),
        ``cursor``, ``keys_only``, ``join``."""
        return self._http.request("POST", f"{self._ns}/collections/{seg(collection)}/query", body=req)

    def query_all(self, collection: str, **req: Any) -> Iterator[Document]:
        """Iterate every matching document across pages (cursor handled for you)."""
        cursor = req.pop("cursor", None)
        while True:
            page = self.query(collection, **req, **({"cursor": cursor} if cursor else {}))
            for doc in page.get("documents") or []:
                yield doc
            cursor = page.get("cursor")
            if not cursor:
                return

    def aggregate(self, collection: str, **req: Any) -> Dict[str, Any]:
        """Grouped metrics over an index-served filter: ``metrics`` (count/sum/
        avg/min/max), ``group``, ``where``, ``order``, ``limit``."""
        return self._http.request("POST", f"{self._ns}/collections/{seg(collection)}/aggregate", body=req)

    def transaction(self, operations: Sequence[Dict[str, Any]]) -> List[Optional[str]]:
        """Apply up to 500 operations (op: put/delete/mutate/check) atomically
        within this namespace. NOT retried automatically (increments would
        double-apply); a failed check raises a 409 ``AltEngineError``. Returns
        the per-op resulting key, ``None`` for delete/check ops."""
        res = self._http.request("POST", f"{self._ns}/transaction", body={"operations": list(operations)}, retry=False)
        return res["keys"]

    # --- indexes ---

    def list_indexes(self, collection: str) -> List[Dict[str, Any]]:
        return self._http.request("GET", f"{self._ns}/collections/{seg(collection)}/indexes")["indexes"]

    def create_index(self, collection: str, fields: Sequence[str], unique: bool = False) -> Dict[str, Any]:
        """Create a secondary index (idempotent). ``fields`` entries are
        ``"field"`` or ``"field:asc"`` / ``"field:desc"``, max 8."""
        res = self._http.request(
            "POST",
            f"{self._ns}/collections/{seg(collection)}/indexes",
            body={"fields": list(fields), "unique": unique},
        )
        return res["index"]

    def delete_index(self, collection: str, index_id: int) -> bool:
        res = self._http.request("DELETE", f"{self._ns}/collections/{seg(collection)}/indexes/{index_id}")
        return res["deleted"]

    # --- namespaces (instance-wide, not bound to this client's namespace) ---

    def list_namespaces(self, q: Optional[str] = None, limit: Optional[int] = None) -> Dict[str, Any]:
        return self._http.request("GET", f"{self._base}/namespaces", query={"q": q, "limit": limit})

    def delete_namespace(self, namespace: str) -> bool:
        """Delete a namespace and everything in it. Requires a ``full`` grant."""
        return self._http.request("DELETE", f"{self._base}/namespaces/{seg(namespace)}")["deleted"]


class AsyncDatastoreClient:
    """Async twin of :class:`DatastoreClient` (same methods, awaitable)."""

    def __init__(self, http: AsyncHttp, instance: str, namespace: str = "") -> None:
        self._http = http
        self.instance = instance
        self.namespace = namespace
        self._base = f"/v1/datastore/{seg(instance)}"
        self._ns = f"{self._base}/namespaces/{seg(namespace)}"

    def with_namespace(self, namespace: str) -> "AsyncDatastoreClient":
        return AsyncDatastoreClient(self._http, self.instance, namespace)

    async def put(self, collection: str, documents: Sequence[Document]) -> List[str]:
        res = await self._http.request(
            "POST", f"{self._ns}/collections/{seg(collection)}/documents", body={"documents": list(documents)}
        )
        return res["keys"]

    async def get(self, collection: str, key: Union[Key, Sequence[Key]]) -> Any:
        single = not isinstance(key, (list, tuple))
        keys = [key] if single else list(key)
        res = await self._http.request(
            "POST", f"{self._ns}/collections/{seg(collection)}/documents/get", body={"keys": keys}
        )
        by_key = {d["key"]: d for d in res["documents"]}
        docs = [by_key.get(_key_str(k)) for k in keys]
        return docs[0] if single else docs

    async def delete(self, collection: str, keys: Sequence[Key]) -> int:
        res = await self._http.request(
            "POST", f"{self._ns}/collections/{seg(collection)}/documents/delete", body={"keys": list(keys)}
        )
        return res["deleted"]

    async def query(self, collection: str, **req: Any) -> Dict[str, Any]:
        return await self._http.request("POST", f"{self._ns}/collections/{seg(collection)}/query", body=req)

    async def query_all(self, collection: str, **req: Any) -> AsyncIterator[Document]:
        cursor = req.pop("cursor", None)
        while True:
            page = await self.query(collection, **req, **({"cursor": cursor} if cursor else {}))
            for doc in page.get("documents") or []:
                yield doc
            cursor = page.get("cursor")
            if not cursor:
                return

    async def aggregate(self, collection: str, **req: Any) -> Dict[str, Any]:
        return await self._http.request("POST", f"{self._ns}/collections/{seg(collection)}/aggregate", body=req)

    async def transaction(self, operations: Sequence[Dict[str, Any]]) -> List[Optional[str]]:
        res = await self._http.request(
            "POST", f"{self._ns}/transaction", body={"operations": list(operations)}, retry=False
        )
        return res["keys"]

    async def list_indexes(self, collection: str) -> List[Dict[str, Any]]:
        return (await self._http.request("GET", f"{self._ns}/collections/{seg(collection)}/indexes"))["indexes"]

    async def create_index(self, collection: str, fields: Sequence[str], unique: bool = False) -> Dict[str, Any]:
        res = await self._http.request(
            "POST",
            f"{self._ns}/collections/{seg(collection)}/indexes",
            body={"fields": list(fields), "unique": unique},
        )
        return res["index"]

    async def delete_index(self, collection: str, index_id: int) -> bool:
        res = await self._http.request("DELETE", f"{self._ns}/collections/{seg(collection)}/indexes/{index_id}")
        return res["deleted"]

    async def list_namespaces(self, q: Optional[str] = None, limit: Optional[int] = None) -> Dict[str, Any]:
        return await self._http.request("GET", f"{self._base}/namespaces", query={"q": q, "limit": limit})

    async def delete_namespace(self, namespace: str) -> bool:
        return (await self._http.request("DELETE", f"{self._base}/namespaces/{seg(namespace)}"))["deleted"]
