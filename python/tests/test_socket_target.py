"""The token goes in a subprotocol, not the URL, whenever there is an endpoint to
dial without it; a response with only ``ws_url`` comes from an older server and
is opened as is."""

from altengine import ChannelSocket

TOK = "a.b.c"
PROTOCOLS = ["altengine.v1", "altengine.token.a.b.c"]
ENDPOINT = "wss://x.test/v1/channel/i/subscribe"


def target(tok, **kw):
    return ChannelSocket(get_token=lambda: tok, **kw)._target(tok)


def test_ws_endpoint_with_protocols():
    assert target({"token": TOK, "ws_endpoint": ENDPOINT, "protocols": PROTOCOLS, "ws_url": ENDPOINT + "?token=" + TOK}) == (
        ENDPOINT,
        PROTOCOLS,
    )


def test_ws_endpoint_without_protocols():
    assert target({"token": TOK, "ws_endpoint": ENDPOINT}) == (ENDPOINT, PROTOCOLS)


def test_older_server_ws_url_only():
    assert target({"token": TOK, "ws_url": ENDPOINT + "?token=" + TOK}) == (ENDPOINT + "?token=" + TOK, None)


def test_explicit_url_sends_token_as_subprotocol():
    url, protocols = target({"token": TOK}, url=ENDPOINT, channels=["r1"])
    assert url == ENDPOINT + "?channels=r1"
    assert TOK not in url
    assert protocols == PROTOCOLS
