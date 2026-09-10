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

"""pulumi_engine: the Pulumi engine as an in-process library.

The engine runs inside this process (libpulumi, loaded through cffi);
provider plugins are child processes as the Pulumi protocol requires. No
``pulumi`` CLI is involved.

    from pulumi_engine import open_stack, Secret, LocalProgram

    stack = open_stack({
        "name": "dev",
        "project": {"name": "demo"},
        "backend": {"url": "file:///var/lib/demo/state"},
        "secrets": {"provider": "passphrase", "passphrase": "..."},
        "config": {"token": Secret("...")},
        "create": True,
    })
    with stack.up(LocalProgram("/path/to/project")) as op:
        for event in op:
            print(event["type"])
        result = op.result()
"""

from ._native import library_path
from ._stack import (
    CallbackProgram,
    LocalProgram,
    Operation,
    Options,
    Secret,
    Stack,
    list_stacks,
    open_stack,
    version,
)
from .errors import (
    CancelledError,
    ConcurrentUpdateError,
    InvalidSpecError,
    PendingOperationsError,
    PlanViolationError,
    ProgramFailedError,
    PulumiError,
    ResourceOpFailedError,
    StackExistsError,
    StackNotFoundError,
    UnsupportedError,
)


def _package_version() -> str:
    """The installed distribution's version (set from the release tag at
    packaging time); ``version()`` reports the library's own."""
    try:
        from importlib.metadata import version as _dist_version

        return _dist_version("pulumi_engine")
    except Exception:  # noqa: BLE001 - not installed as a distribution
        return "0.0.0.dev0"


__version__ = _package_version()

__all__ = [
    "CallbackProgram",
    "CancelledError",
    "ConcurrentUpdateError",
    "InvalidSpecError",
    "LocalProgram",
    "Operation",
    "Options",
    "PendingOperationsError",
    "PlanViolationError",
    "ProgramFailedError",
    "PulumiError",
    "ResourceOpFailedError",
    "Secret",
    "Stack",
    "StackExistsError",
    "StackNotFoundError",
    "UnsupportedError",
    "library_path",
    "list_stacks",
    "open_stack",
    "version",
]
