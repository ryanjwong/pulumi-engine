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

from setuptools import setup

setup(version=os.environ.get("PULUMI_ENGINE_VERSION") or "0.0.0.dev0")
