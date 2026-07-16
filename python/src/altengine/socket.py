"""ChannelSocket — the managed asyncio WebSocket subscriber for altengine
channels. Requires the ``ws`` extra (``pip install altengine[ws]``).

Your code supplies ``get_token`` (typically ``ChannelClient.create_token`` or a
call to your own backend); the socket does the rest — connect, auto-reconnect
with backoff, token re-mint on reconnect, resubscribe, and keepalive pings.
"""

from __future__ import annotations

import asyncio
import inspect
import json
import random
from typing import Any, Awaitable, Callable, Dict, List, Optional, Sequence, Union
from urllib.parse import parse_qs, urlencode, urlsplit, urlunsplit

TokenProvider = Callable[[], Union[Dict[str, Any], Awaitable[Dict[str, Any]]]]


class ChannelSocketError(Exception):
    """A server error frame or lifecycle failure on the socket."""


class ChannelSocket:
    """Managed WebSocket subscriber.

    ``get_token`` is called before every connection attempt (including
    reconnects) so tokens are always fresh; it returns the token-mint response
    (``{token, ws_url, ...}``). Messages arrive on :meth:`messages` (an async
    iterator) and/or the ``on_message`` callback.
    """

    def __init__(
        self,
        get_token: TokenProvider,
        url: Optional[str] = None,
        channels: Optional[Sequence[str]] = None,
        on_message: Optional[Callable[[Dict[str, Any]], None]] = None,
        ping_interval: float = 30.0,
        backoff_base: float = 0.25,
        backoff_max: float = 30.0,
    ) -> None:
        self._get_token = get_token
        self._url = url
        self._on_message = on_message
        self._ping_interval = ping_interval
        self._backoff_base = backoff_base
        self._backoff_max = backoff_max

        self.state = "idle"
        self._explicit = channels is not None
        self._desired: set = set(channels or [])
        self._ws: Any = None
        self._closed = False
        self._attempts = 0
        self._tasks: List[asyncio.Task] = []
        self._queue: asyncio.Queue = asyncio.Queue()
        self._pending_subs: List[tuple] = []  # (channels, future)
        self._pending_pubs: List[asyncio.Future] = []
        self._last_activity = 0.0

    async def connect(self) -> None:
        """Dial the socket; returns once the connection is open. After a
        successful connect the socket reconnects automatically until
        :meth:`close`."""
        if self._ws is not None:
            return
        self._closed = False
        self.state = "connecting"
        await self._dial()

    async def close(self) -> None:
        """Stop reconnecting and close the connection."""
        self._closed = True
        self.state = "closed"
        self._fail_pending(ChannelSocketError("socket closed"))
        for t in self._tasks:
            t.cancel()
        self._tasks = []
        ws, self._ws = self._ws, None
        if ws is not None:
            await ws.close()

    async def subscribe(self, channels: Sequence[str]) -> List[str]:
        """Subscribe to more channels (tracked and re-applied after
        reconnects); resolves with the server's ack."""
        self._desired.update(channels)
        self._explicit = True
        if self._ws is None or self.state != "open":
            return sorted(self._desired)
        fut: asyncio.Future = asyncio.get_running_loop().create_future()
        self._pending_subs.append((list(channels), fut))
        await self._ws.send(json.dumps({"type": "subscribe", "channels": list(channels)}))
        return await fut

    async def unsubscribe(self, channels: Sequence[str]) -> None:
        self._desired.difference_update(channels)
        self._explicit = True
        if self._ws is not None and self.state == "open":
            await self._ws.send(json.dumps({"type": "unsubscribe", "channels": list(channels)}))

    async def publish(self, channel: str, data: Any) -> int:
        """Publish over the socket (requires a ws-capable ``publish`` token);
        resolves with the delivered count from the server's ack."""
        if self._ws is None or self.state != "open":
            raise ChannelSocketError("socket is not open")
        fut: asyncio.Future = asyncio.get_running_loop().create_future()
        self._pending_pubs.append(fut)
        await self._ws.send(json.dumps({"type": "publish", "channel": channel, "data": data}))
        return await fut

    async def messages(self):
        """Async iterator over delivered messages (``{channel, data, ts}``)."""
        while not self._closed:
            msg = await self._queue.get()
            if msg is None:
                return
            yield msg

    # --- internals ---

    async def _token(self) -> Dict[str, Any]:
        tok = self._get_token()
        if inspect.isawaitable(tok):
            tok = await tok
        return tok

    def _build_url(self, tok: Dict[str, Any]) -> str:
        raw = self._url or tok.get("ws_url")
        if not raw:
            raise ChannelSocketError("no WebSocket URL: token response had no ws_url and no url was set")
        parts = urlsplit(raw)
        query = parse_qs(parts.query)
        if "token" not in query:
            query["token"] = [tok["token"]]
        # Narrow the initial subscription to the explicitly requested subset.
        if self._explicit and self._desired:
            query["channels"] = [",".join(sorted(self._desired))]
        return urlunsplit(parts._replace(query=urlencode(query, doseq=True)))

    async def _dial(self) -> None:
        import websockets

        tok = await self._token()
        ws = await websockets.connect(self._build_url(tok), max_size=1 << 20)
        self._ws = ws
        self._attempts = 0
        self._last_activity = asyncio.get_running_loop().time()
        self.state = "open"
        self._tasks = [
            asyncio.create_task(self._read_loop(ws)),
            asyncio.create_task(self._ping_loop(ws)),
        ]

    async def _read_loop(self, ws: Any) -> None:
        try:
            async for raw in ws:
                self._last_activity = asyncio.get_running_loop().time()
                self._handle_frame(raw)
        except Exception:
            pass
        finally:
            if self._ws is ws:
                await self._on_disconnect()

    def _handle_frame(self, raw: Any) -> None:
        if raw in ("ping", "pong"):
            return
        try:
            frame = json.loads(raw)
        except (ValueError, TypeError):
            return
        if not isinstance(frame, dict):
            return
        ftype = frame.get("type")
        if ftype == "subscribed":
            # Only settle the oldest pending subscribe if this ack covers its
            # channels — an unsolicited ack (e.g. for the connect-time
            # ?channels= subscription) must not steal a subscribe()'s resolution.
            acked = set(frame.get("channels") or [])
            if self._pending_subs and set(self._pending_subs[0][0]) <= acked:
                _, fut = self._pending_subs.pop(0)
                if not fut.done():
                    fut.set_result(frame.get("channels") or [])
        elif ftype == "published":
            if self._pending_pubs:
                fut = self._pending_pubs.pop(0)
                if not fut.done():
                    fut.set_result(frame.get("delivered", 0))
        elif ftype == "error":
            err = ChannelSocketError(str(frame.get("error")))
            # An error ack settles the oldest pending request, if any.
            if self._pending_pubs:
                fut = self._pending_pubs.pop(0)
                if not fut.done():
                    fut.set_exception(err)
            elif self._pending_subs:
                _, fut = self._pending_subs.pop(0)
                if not fut.done():
                    fut.set_exception(err)
        elif ftype is None and isinstance(frame.get("channel"), str):
            if self._on_message is not None:
                self._on_message(frame)
            self._queue.put_nowait(frame)

    async def _ping_loop(self, ws: Any) -> None:
        try:
            while True:
                await asyncio.sleep(self._ping_interval)
                now = asyncio.get_running_loop().time()
                if now - self._last_activity > self._ping_interval * 1.5:
                    # Dead connection: no traffic (not even pongs). Force a reconnect.
                    await ws.close(code=4000, reason="keepalive timeout")
                    return
                await ws.send("ping")
        except Exception:
            pass

    async def _on_disconnect(self) -> None:
        self._ws = None
        self._fail_pending(ChannelSocketError("socket closed"))
        if self._closed:
            return
        self.state = "reconnecting"
        delay = min(self._backoff_base * 2 ** min(self._attempts, 20), self._backoff_max)
        self._attempts += 1
        # Full jitter keeps concurrent reconnects from stampeding in unison.
        await asyncio.sleep(delay * (0.5 + random.random() * 0.5))
        if self._closed or self._ws is not None:
            return
        try:
            await self._dial()
        except Exception:
            await self._on_disconnect()

    def _fail_pending(self, err: Exception) -> None:
        for _, fut in self._pending_subs:
            if not fut.done():
                fut.set_exception(err)
        self._pending_subs = []
        for fut in self._pending_pubs:
            if not fut.done():
                fut.set_exception(err)
        self._pending_pubs = []
