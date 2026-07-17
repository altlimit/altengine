"""Search clients (sync + async). All payloads are wire-verbatim dicts."""

from __future__ import annotations

from typing import Any, AsyncIterator, Dict, Iterator, List, Optional, Sequence, Union

from ._http import AsyncHttp, Http, seg, ns_seg

Document = Dict[str, Any]


class SearchClient:
    """Client for one search instance, bound to a namespace (carried in the
    request path, default ``""``)."""

    def __init__(self, http: Http, instance: str, namespace: str = "") -> None:
        self._http = http
        self.instance = instance
        self.namespace = namespace
        self._base = f"/v1/search/{seg(instance)}"
        self._ns = f"{self._base}/ns/{ns_seg(namespace)}"

    def with_namespace(self, namespace: str) -> "SearchClient":
        """Same instance, different namespace."""
        return SearchClient(self._http, self.instance, namespace)

    def index(self, name: str) -> "SearchIndex":
        return SearchIndex(self._http, self._ns, name)

    def list_indexes(self, q: Optional[str] = None, limit: Optional[int] = None) -> Dict[str, Any]:
        return self._http.request("GET", f"{self._ns}/idx", query={"q": q, "limit": limit})

    def list_namespaces(self, q: Optional[str] = None, limit: Optional[int] = None) -> Dict[str, Any]:
        """Distinct namespaces with live indexes — alphabetical, ``q`` substring
        search, ``limit`` default 50 (max 100). Instance-wide; the default
        namespace appears as ``""``."""
        return self._http.request("GET", f"{self._base}/ns", query={"q": q, "limit": limit})

    def delete_index(self, name: str) -> bool:
        """Delete an index and all its documents. Requires a ``full`` grant."""
        res = self._http.request("DELETE", f"{self._ns}/idx/{seg(name)}")
        return res["deleted"]


class SearchIndex:
    """Operations on one search index."""

    def __init__(self, http: Http, ns_base: str, name: str) -> None:
        self._http = http
        self.name = name
        self._path = f"{ns_base}/idx/{seg(name)}"

    def put(self, documents: Sequence[Document]) -> List[str]:
        """Upsert up to 200 documents (see :mod:`altengine.fields` builders).
        Returns their ids in order (server-assigned when a document omits
        ``id``)."""
        res = self._http.request("POST", f"{self._path}/documents", body={"documents": list(documents)})
        return res["ids"]

    def get(self, doc_id: Union[str, Sequence[str]]) -> Any:
        """Fetch one document (``None`` when missing), or — passed a list — up
        to 200 documents in one round trip, order-preserving with ``None``
        placeholders for missing ids. Both forms ride the batch endpoint."""
        single = isinstance(doc_id, str)
        ids = [doc_id] if single else list(doc_id)
        res = self._http.request("POST", f"{self._path}/documents/get", body={"ids": ids})
        by_id = {d["id"]: d for d in res["documents"]}
        docs = [by_id.get(i) for i in ids]
        return docs[0] if single else docs

    def delete(self, ids: Sequence[str]) -> int:
        """Delete up to 200 documents by id; missing ids are no-ops. Requires
        a ``full`` grant."""
        res = self._http.request("POST", f"{self._path}/documents/delete", body={"ids": list(ids)})
        return res["deleted"]

    def search(self, query: str = "", **req: Any) -> Dict[str, Any]:
        """Run a search request. ``query`` is the boolean query language;
        keyword args are the wire request (``limit``, ``cursor``, ``sort``,
        ``facets``, ``snippet``, ``collapse``, ``ids_only``, ...)."""
        return self._http.request("POST", f"{self._path}/search", body={"query": query, **req})

    def search_all(self, query: str = "", **req: Any) -> Iterator[Dict[str, Any]]:
        """Iterate every hit across cursor pages."""
        req.pop("offset", None)
        cursor = req.pop("cursor", None)
        while True:
            page = self.search(query, **req, **({"cursor": cursor} if cursor else {}))
            for hit in page["results"]:
                yield hit
            cursor = page.get("cursor")
            if not cursor:
                return

    def list_documents(
        self,
        start_id: Optional[str] = None,
        include_start: Optional[bool] = None,
        limit: Optional[int] = None,
        ids_only: Optional[bool] = None,
    ) -> Dict[str, Any]:
        """One page of documents in id order (keyset pagination via ``start_id``)."""
        return self._http.request(
            "GET",
            f"{self._path}/documents",
            query={"start_id": start_id, "include_start": include_start, "limit": limit, "ids_only": ids_only},
        )

    def list_all_documents(self, limit: Optional[int] = None) -> Iterator[Document]:
        """Iterate every document in the index (keyset pagination handled for you)."""
        start_id: Optional[str] = None
        while True:
            page = self.list_documents(start_id=start_id, include_start=start_id is None, limit=limit)
            docs = page.get("documents") or []
            if not docs:
                return
            for doc in docs:
                yield doc
            start_id = docs[-1]["id"]

    def schema(self) -> Dict[str, Any]:
        """The index's union field schema: for every field name, the types it
        has been indexed with."""
        return self._http.request("GET", f"{self._path}/schema")


class AsyncSearchClient:
    """Async twin of :class:`SearchClient`."""

    def __init__(self, http: AsyncHttp, instance: str, namespace: str = "") -> None:
        self._http = http
        self.instance = instance
        self.namespace = namespace
        self._base = f"/v1/search/{seg(instance)}"
        self._ns = f"{self._base}/ns/{ns_seg(namespace)}"

    def with_namespace(self, namespace: str) -> "AsyncSearchClient":
        return AsyncSearchClient(self._http, self.instance, namespace)

    def index(self, name: str) -> "AsyncSearchIndex":
        return AsyncSearchIndex(self._http, self._ns, name)

    async def list_indexes(self, q: Optional[str] = None, limit: Optional[int] = None) -> Dict[str, Any]:
        return await self._http.request("GET", f"{self._ns}/idx", query={"q": q, "limit": limit})

    async def list_namespaces(self, q: Optional[str] = None, limit: Optional[int] = None) -> Dict[str, Any]:
        return await self._http.request("GET", f"{self._base}/ns", query={"q": q, "limit": limit})

    async def delete_index(self, name: str) -> bool:
        res = await self._http.request("DELETE", f"{self._ns}/idx/{seg(name)}")
        return res["deleted"]


class AsyncSearchIndex:
    """Async twin of :class:`SearchIndex`."""

    def __init__(self, http: AsyncHttp, ns_base: str, name: str) -> None:
        self._http = http
        self.name = name
        self._path = f"{ns_base}/idx/{seg(name)}"

    async def put(self, documents: Sequence[Document]) -> List[str]:
        res = await self._http.request("POST", f"{self._path}/documents", body={"documents": list(documents)})
        return res["ids"]

    async def get(self, doc_id: Union[str, Sequence[str]]) -> Any:
        single = isinstance(doc_id, str)
        ids = [doc_id] if single else list(doc_id)
        res = await self._http.request("POST", f"{self._path}/documents/get", body={"ids": ids})
        by_id = {d["id"]: d for d in res["documents"]}
        docs = [by_id.get(i) for i in ids]
        return docs[0] if single else docs

    async def delete(self, ids: Sequence[str]) -> int:
        res = await self._http.request("POST", f"{self._path}/documents/delete", body={"ids": list(ids)})
        return res["deleted"]

    async def search(self, query: str = "", **req: Any) -> Dict[str, Any]:
        return await self._http.request("POST", f"{self._path}/search", body={"query": query, **req})

    async def search_all(self, query: str = "", **req: Any) -> AsyncIterator[Dict[str, Any]]:
        req.pop("offset", None)
        cursor = req.pop("cursor", None)
        while True:
            page = await self.search(query, **req, **({"cursor": cursor} if cursor else {}))
            for hit in page["results"]:
                yield hit
            cursor = page.get("cursor")
            if not cursor:
                return

    async def list_documents(
        self,
        start_id: Optional[str] = None,
        include_start: Optional[bool] = None,
        limit: Optional[int] = None,
        ids_only: Optional[bool] = None,
    ) -> Dict[str, Any]:
        return await self._http.request(
            "GET",
            f"{self._path}/documents",
            query={"start_id": start_id, "include_start": include_start, "limit": limit, "ids_only": ids_only},
        )

    async def list_all_documents(self, limit: Optional[int] = None) -> AsyncIterator[Document]:
        start_id: Optional[str] = None
        while True:
            page = await self.list_documents(start_id=start_id, include_start=start_id is None, limit=limit)
            docs = page.get("documents") or []
            if not docs:
                return
            for doc in docs:
                yield doc
            start_id = docs[-1]["id"]

    async def schema(self) -> Dict[str, Any]:
        return await self._http.request("GET", f"{self._path}/schema")
