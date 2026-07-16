from __future__ import annotations

import asyncio
import time

import pytest

from altengine import ChannelSocket, ChannelSocketError

from .conftest import uniq


@pytest.fixture(scope="module")
def ch(client):
    return client.channel(uniq("sdk-conf"))


class TestTokensAndPublish:
    def test_token_mint(self, ch):
        tok = ch.create_token(channels=["room:1", "room:2"], ttl_seconds=600)
        assert len(tok["token"].split(".")) == 3
        assert tok["channels"] == ["room:1", "room:2"]
        assert "/subscribe" in tok["ws_url"]
        assert tok["expires_at"] > time.time()

    def test_http_publish_delivered_count(self, ch):
        assert ch.publish("room:empty", {"hello": 1}) == 0


async def _wait_message(sock: ChannelSocket, timeout: float = 10.0):
    async def first():
        async for msg in sock.messages():
            return msg

    return await asyncio.wait_for(first(), timeout)


class TestWebSocketLifecycle:
    def test_subscribe_and_receive_http_publish(self, ch):
        async def run():
            sock = ChannelSocket(get_token=lambda: ch.create_token(channels=["room:live"]))
            try:
                await sock.connect()
                assert sock.state == "open"
                # Publish may race the subscribe registration; retry until delivered.
                for i in range(50):
                    if ch.publish("room:live", {"n": i}) > 0:
                        break
                    await asyncio.sleep(0.1)
                msg = await _wait_message(sock)
                assert msg["channel"] == "room:live"
                assert msg["data"]["n"] >= 0
                assert msg["ts"] > 0
            finally:
                await sock.close()

        asyncio.run(run())

    def test_publish_over_socket_with_ws_token(self, ch):
        async def run():
            a = ChannelSocket(get_token=lambda: ch.create_token(channels=["room:ws"], publish="ws"))
            b = ChannelSocket(get_token=lambda: ch.create_token(channels=["room:ws"]))
            try:
                await a.connect()
                await b.connect()
                delivered = 0
                for _ in range(50):
                    delivered = await a.publish("room:ws", {"via": "ws"})
                    if delivered >= 2:
                        break
                    await asyncio.sleep(0.1)
                assert delivered >= 2  # a + b are both subscribed
                msg = await _wait_message(b)
                assert msg["data"]["via"] == "ws"
            finally:
                await a.close()
                await b.close()

        asyncio.run(run())

    def test_runtime_subscribe_unsubscribe_acks(self, ch):
        async def run():
            sock = ChannelSocket(
                get_token=lambda: ch.create_token(channels=["room:a", "room:b"]),
                channels=["room:a"],
            )
            try:
                await sock.connect()
                acked = await sock.subscribe(["room:b"])
                assert "room:b" in acked
                await sock.unsubscribe(["room:a"])
            finally:
                await sock.close()

        asyncio.run(run())

    def test_subscriber_only_socket_cannot_publish(self, ch):
        async def run():
            sock = ChannelSocket(get_token=lambda: ch.create_token(channels=["room:x"]))
            try:
                await sock.connect()
                with pytest.raises(ChannelSocketError):
                    await sock.publish("room:x", {"nope": True})
            finally:
                await sock.close()

        asyncio.run(run())
