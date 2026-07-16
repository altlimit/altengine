"""Conformance harness: builds the emulator once and spawns it on a free port
for the session. When ALTENGINE_CONFORMANCE_URL is set, the suite targets that
instead and no emulator is spawned."""

from __future__ import annotations

import json
import os
import pathlib
import random
import socket
import subprocess
import tempfile
import time

import httpx
import pytest

from altengine import AltEngine

REPO_ROOT = pathlib.Path(__file__).resolve().parents[3]


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


@pytest.fixture(scope="session")
def base_url():
    external = os.environ.get("ALTENGINE_CONFORMANCE_URL")
    if external:
        yield external
        return

    bin_path = pathlib.Path(tempfile.gettempdir()) / "altengine-conformance-py"
    subprocess.run(
        ["go", "build", "-o", str(bin_path), "./cmd/altengine"],
        cwd=REPO_ROOT / "cli",
        check=True,
    )
    port = _free_port()
    child = subprocess.Popen(
        [str(bin_path), "dev", "--memory", "--port", str(port)],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    url = f"http://127.0.0.1:{port}"
    deadline = time.time() + 15
    while True:
        try:
            if httpx.get(f"{url}/healthz").status_code == 200:
                break
        except httpx.HTTPError:
            pass
        if time.time() > deadline:
            child.kill()
            raise RuntimeError("emulator did not become healthy in 15s")
        time.sleep(0.1)
    yield url
    child.terminate()


@pytest.fixture(scope="session")
def api_key() -> str:
    if os.environ.get("ALTENGINE_CONFORMANCE_URL"):
        return os.environ.get("ALTENGINE_CONFORMANCE_KEY", "")
    return "conformance-dev-key"


@pytest.fixture(scope="session")
def client(base_url, api_key) -> AltEngine:
    return AltEngine(api_key=api_key, base_url=base_url)


@pytest.fixture(scope="session")
def destructive_ok() -> bool:
    return (
        not os.environ.get("ALTENGINE_CONFORMANCE_URL")
        or os.environ.get("ALTENGINE_CONFORMANCE_DESTRUCTIVE") == "1"
    )


def uniq(prefix: str) -> str:
    """Random suffix so suites never collide across runs or languages."""
    return f"{prefix}-{int(time.time() * 1000):x}{random.randrange(1 << 16):04x}"


def load_fixture(name: str):
    return json.loads((REPO_ROOT / "conformance" / "fixtures" / name).read_text())
