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

"""The package version comes from the release tag (Makefile VERSION ->
PULUMI_ENGINE_VERSION), as it does for the library and the Node packages;
everything else is in pyproject.toml. A checkout without a version is
0.0.0.dev0."""

import os
import re

from setuptools import setup


def pep440(version: str) -> str:
    """Map a tag-style version (``1.2.3``, ``1.2.3-rc.1``, ``0.0.0-ci``) to
    PEP 440: pre-release labels become ``1.2.3rc1``; any other label becomes
    a dev release with the label as local version (``0.0.0.dev0+ci``)."""
    m = re.fullmatch(r"(\d+\.\d+\.\d+)(?:-(.+))?", version)
    if not m:
        return version  # let setuptools validate whatever this is
    base, label = m.group(1), m.group(2)
    if not label:
        return base
    pre = re.fullmatch(r"(a|b|rc|alpha|beta|pre|preview)\.?(\d+)", label)
    if pre:
        kind = {"alpha": "a", "beta": "b", "pre": "rc", "preview": "rc"}.get(pre.group(1), pre.group(1))
        return f"{base}{kind}{pre.group(2)}"
    local = re.sub(r"[^a-zA-Z0-9.]+", ".", label).strip(".")
    return f"{base}.dev0+{local}" if local else f"{base}.dev0"


setup(version=pep440(os.environ.get("PULUMI_ENGINE_VERSION") or "0.0.0.dev0"))
