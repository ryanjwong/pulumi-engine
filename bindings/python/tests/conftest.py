# Copyright 2026 Ryan Wong
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Shared fixtures: the built library, the YAML fixtures from engine/testdata
and a file:// backend per test. The "local" tests run the YAML language host
and the random/command providers (downloaded on first use)."""

from __future__ import annotations

import os
import shutil
import tempfile
from pathlib import Path
from typing import Any, Callable

import pytest

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[2]
FIXTURES = REPO / "engine" / "testdata"
PASSPHRASE = "correct horse battery staple"
# The pulumi-language-yaml release the tests pin, as engine/integration_test.go does.
YAML_LANGUAGE_VERSION = "1.38.5"


def pytest_configure() -> None:
    if not os.environ.get("PULUMI_ENGINE_LIB"):
        for ext in ("dylib", "so"):
            lib = REPO / "build" / f"libpulumi.{ext}"
            if lib.is_file():
                os.environ["PULUMI_ENGINE_LIB"] = str(lib)
                break
    os.environ.setdefault("PULUMI_SKIP_UPDATE_CHECK", "true")


@pytest.fixture
def tmp(tmp_path: Path) -> Callable[[str], str]:
    """A fresh directory under the test's temp root."""

    def make(prefix: str) -> str:
        return tempfile.mkdtemp(prefix=f"pulumi-engine-{prefix}-", dir=tmp_path)

    return make


@pytest.fixture
def copy_fixture(tmp: Callable[[str], str]) -> Callable[[str], str]:
    """Copy engine/testdata/<name> to a temp dir so Pulumi.<stack>.yaml can be
    written next to it."""

    def copy(name: str) -> str:
        dst = tmp(name)
        for f in (FIXTURES / name).iterdir():
            shutil.copy(f, Path(dst) / f.name)
        return dst

    return copy


@pytest.fixture
def spec(tmp: Callable[[str], str]) -> Callable[..., dict[str, Any]]:
    """A StackSpec on a private file:// backend with passphrase secrets."""

    def make(name: str, dir: str | None = None, **extra: Any) -> dict[str, Any]:
        s: dict[str, Any] = {
            "name": name,
            "project": {"name": "engine-yaml", "dir": dir} if dir else {"name": "py-test", "dir": tmp("proj")},
            "backend": {"url": "file://" + tmp("state")},
            "secrets": {"provider": "passphrase", "passphrase": PASSPHRASE},
            "create": True,
        }
        s.update(extra)
        return s

    return make


def local(dir: str) -> dict[str, Any]:
    """The wire form of the YAML fixture program, host version pinned."""
    return {"mode": "local", "dir": dir, "languageVersion": YAML_LANGUAGE_VERSION}


def types(events: list[dict[str, Any]]) -> list[str]:
    return [e["type"] for e in events]
