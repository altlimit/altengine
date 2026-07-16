"""Channel (realtime pub/sub) clients — server-side token minting, HTTP
publish, and presence. For the managed WebSocket subscriber see
:class:`altengine.ChannelSocket` (requires the ``ws`` extra)."""

from __future__ import annotations

from typing import Any, Dict, Optional, Sequence, Union

from ._http import AsyncHttp, Http, seg

PublishMode = Union[bool, str]  # True ≡ "all"; "http" | "ws" | "all"


class ChannelClient:
    """Server-side client for one channel instance (API-key auth)."""

    def __init__(self, http: Http, instance: str) -> None:
        self._http = http
        self.instance = instance
        self._base = f"/v1/channel/{seg(instance)}"

    def create_token(
        self,
        channels: Sequence[str],
        ttl_seconds: Optional[int] = None,
        publish: Optional[PublishMode] = None,
        presence_id: Optional[str] = None,
    ) -> Dict[str, Any]:
        """Mint a subscriber token (JWT) for the given channels (≤100 names,
        each ≤200 bytes). ``ttl_seconds`` defaults to 3600 (max 14400);
        ``publish`` grants publish capability (requires a ``write`` grant);
        ``presence_id`` (≤128 bytes) is bound into the token server-side.
        Returns ``{token, expires_at, channels, publish, ws_url, ...}``."""
        body: Dict[str, Any] = {"channels": list(channels)}
        if ttl_seconds is not None:
            body["ttl_seconds"] = ttl_seconds
        if publish is not None:
            body["publish"] = publish
        if presence_id is not None:
            body["presence_id"] = presence_id
        return self._http.request("POST", f"{self._base}/tokens", body=body)

    def publish(self, channel: str, data: Any) -> int:
        """Publish a message (framed ≤32 KiB) to a channel; returns the number
        of subscribers reached. NOT retried automatically (a retry
        double-delivers)."""
        res = self._http.request(
            "POST", f"{self._base}/publish", body={"channel": channel, "data": data}, retry=False
        )
        return res["delivered"]

    def presence(self, channel: str) -> Dict[str, Any]:
        """Presence roster for a channel (requires the instance's presence flag)."""
        return self._http.request("GET", f"{self._base}/presence", query={"channel": channel})


class AsyncChannelClient:
    """Async twin of :class:`ChannelClient`."""

    def __init__(self, http: AsyncHttp, instance: str) -> None:
        self._http = http
        self.instance = instance
        self._base = f"/v1/channel/{seg(instance)}"

    async def create_token(
        self,
        channels: Sequence[str],
        ttl_seconds: Optional[int] = None,
        publish: Optional[PublishMode] = None,
        presence_id: Optional[str] = None,
    ) -> Dict[str, Any]:
        body: Dict[str, Any] = {"channels": list(channels)}
        if ttl_seconds is not None:
            body["ttl_seconds"] = ttl_seconds
        if publish is not None:
            body["publish"] = publish
        if presence_id is not None:
            body["presence_id"] = presence_id
        return await self._http.request("POST", f"{self._base}/tokens", body=body)

    async def publish(self, channel: str, data: Any) -> int:
        res = await self._http.request(
            "POST", f"{self._base}/publish", body={"channel": channel, "data": data}, retry=False
        )
        return res["delivered"]

    async def presence(self, channel: str) -> Dict[str, Any]:
        return await self._http.request("GET", f"{self._base}/presence", query={"channel": channel})
