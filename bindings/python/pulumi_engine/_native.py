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

"""The cffi layer: loads libpulumi (ABI mode, no compile step) and exposes
its C ABI as plain Python functions.

Strings returned by the library are decoded and released with
``pulumi_free`` here, so nothing above this module sees a raw pointer.
cffi releases the GIL around every foreign call, so the two blocking calls
(``op_next_event``, ``op_wait``) never stall other Python threads.
"""

from __future__ import annotations

import os
import platform
import sys
import threading
from pathlib import Path
from typing import Optional

from cffi import FFI

from .errors import InvalidSpecError, PulumiError, error_from_json

# The C ABI (build/libpulumi.h, minus the cgo preamble).
_CDEF = """
char* pulumi_version(void);
void pulumi_free(char* p);
int64_t pulumi_stack_open(char* specJSON, char** err);
int pulumi_stack_close(int64_t h);
int64_t pulumi_op_start(int64_t h, char* requestJSON, char** err);
char* pulumi_op_next_event(int64_t id, int32_t timeoutMs);
int pulumi_op_cancel(int64_t id);
char* pulumi_op_wait(int64_t id, char** err);
int pulumi_op_release(int64_t id);
char* pulumi_stack_export(int64_t h, char** err);
int pulumi_stack_import(int64_t h, char* deploymentJSON, char** err);
char* pulumi_stack_outputs(int64_t h, int showSecrets, char** err);
int pulumi_stack_set_config(int64_t h, char* key, char* valueJSON, char** err);
char* pulumi_stack_get_config(int64_t h, char* key, char** err);
int pulumi_stack_remove(int64_t h, int force, char** err);
int pulumi_stack_cancel(int64_t h, char** err);
char* pulumi_stack_get_tags(int64_t h, char** err);
int pulumi_stack_set_tags(int64_t h, char* tagsJSON, char** err);
char* pulumi_stack_history(int64_t h, char* optionsJSON, char** err);
char* pulumi_list_stacks(char* requestJSON, char** err);
"""


def _platform_tag() -> tuple[str, str, str]:
    """(os, goarch, extension) as the release artifacts name them."""
    system = "darwin" if sys.platform == "darwin" else "linux"
    machine = platform.machine().lower()
    arch = {"x86_64": "amd64", "amd64": "amd64", "arm64": "arm64", "aarch64": "arm64"}.get(machine, machine)
    return system, arch, ("dylib" if system == "darwin" else "so")


def library_path() -> str:
    """Locate the shared library.

    Order: ``$PULUMI_ENGINE_LIB``; the library bundled in this package under
    ``native/``; ``build/libpulumi.<ext>`` of a repository checkout that
    contains this package.
    """
    override = os.environ.get("PULUMI_ENGINE_LIB")
    if override:
        return override
    system, arch, ext = _platform_tag()
    here = Path(__file__).resolve().parent
    candidates = [here / "native" / f"libpulumi-{system}-{arch}.{ext}"]
    for parent in here.parents:
        candidates.append(parent / "build" / f"libpulumi.{ext}")
        if (parent / "go.mod").exists():
            break
    for c in candidates:
        if c.is_file():
            return str(c)
    tried = ", ".join(str(c) for c in candidates)
    raise PulumiError(
        "unclassified",
        f"libpulumi not found for {system}/{arch}; looked in {tried}. "
        "Build it with 'make lib', install the platform library into pulumi_engine/native/, "
        "or set PULUMI_ENGINE_LIB.",
    )


class Native:
    """The loaded library. One instance per process (see :func:`native`)."""

    def __init__(self, path: str) -> None:
        self.ffi = FFI()
        self.ffi.cdef(_CDEF)
        self.lib = self.ffi.dlopen(path)
        self.path = path

    # -- helpers ---------------------------------------------------------

    def _take(self, p: object) -> Optional[str]:
        """Decode and free a string the library returned; None for NULL."""
        if p == self.ffi.NULL:
            return None
        try:
            return self.ffi.string(p).decode("utf-8")  # type: ignore[arg-type]
        finally:
            self.lib.pulumi_free(p)

    def _check(self, err: object) -> None:
        """Raise the typed error held in an out-parameter, if any."""
        text = self._take(err[0])  # type: ignore[index]
        if text is not None:
            raise error_from_json(text)

    def _err(self) -> object:
        return self.ffi.new("char**")

    @staticmethod
    def _b(s: str) -> bytes:
        return s.encode("utf-8")

    # -- the ABI ---------------------------------------------------------

    def version(self) -> str:
        return self._take(self.lib.pulumi_version()) or ""

    def stack_open(self, spec_json: str) -> int:
        err = self._err()
        h = self.lib.pulumi_stack_open(self._b(spec_json), err)
        self._check(err)
        return int(h)

    def stack_close(self, handle: int) -> None:
        if self.lib.pulumi_stack_close(handle) != 0:
            raise InvalidSpecError("stack", f"unknown stack handle {handle} (already closed?)")

    def op_start(self, handle: int, request_json: str) -> int:
        err = self._err()
        op = self.lib.pulumi_op_start(handle, self._b(request_json), err)
        self._check(err)
        return int(op)

    def op_next_event(self, op: int, timeout_ms: int) -> Optional[str]:
        """The event JSON, "" on timeout, or None at end of stream. Blocks
        without the GIL."""
        return self._take(self.lib.pulumi_op_next_event(op, timeout_ms))

    def op_cancel(self, op: int) -> None:
        if self.lib.pulumi_op_cancel(op) != 0:
            raise InvalidSpecError("operation", f"unknown operation {op} (already released?)")

    def op_wait(self, op: int) -> str:
        """The result JSON; raises the typed error (carrying the partial
        result) on failure. Blocks without the GIL."""
        err = self._err()
        p = self.lib.pulumi_op_wait(op, err)
        res = self._take(p)
        self._check(err)
        return res or "{}"

    def op_release(self, op: int) -> None:
        self.lib.pulumi_op_release(op)

    def stack_export(self, handle: int) -> str:
        err = self._err()
        s = self._take(self.lib.pulumi_stack_export(handle, err))
        self._check(err)
        return s or ""

    def stack_import(self, handle: int, deployment_json: str) -> None:
        err = self._err()
        self.lib.pulumi_stack_import(handle, self._b(deployment_json), err)
        self._check(err)

    def stack_outputs(self, handle: int, show_secrets: bool) -> str:
        err = self._err()
        s = self._take(self.lib.pulumi_stack_outputs(handle, 1 if show_secrets else 0, err))
        self._check(err)
        return s or ""

    def stack_set_config(self, handle: int, key: str, value_json: str) -> None:
        err = self._err()
        self.lib.pulumi_stack_set_config(handle, self._b(key), self._b(value_json), err)
        self._check(err)

    def stack_get_config(self, handle: int, key: str) -> str:
        err = self._err()
        s = self._take(self.lib.pulumi_stack_get_config(handle, self._b(key), err))
        self._check(err)
        return s or "null"

    def stack_remove(self, handle: int, force: bool) -> None:
        err = self._err()
        self.lib.pulumi_stack_remove(handle, 1 if force else 0, err)
        self._check(err)

    def stack_cancel(self, handle: int) -> None:
        err = self._err()
        self.lib.pulumi_stack_cancel(handle, err)
        self._check(err)

    def stack_get_tags(self, handle: int) -> str:
        err = self._err()
        s = self._take(self.lib.pulumi_stack_get_tags(handle, err))
        self._check(err)
        return s or "{}"

    def stack_set_tags(self, handle: int, tags_json: str) -> None:
        err = self._err()
        self.lib.pulumi_stack_set_tags(handle, self._b(tags_json), err)
        self._check(err)

    def stack_history(self, handle: int, options_json: str) -> str:
        err = self._err()
        s = self._take(self.lib.pulumi_stack_history(handle, self._b(options_json), err))
        self._check(err)
        return s or "[]"

    def list_stacks(self, request_json: str) -> str:
        err = self._err()
        s = self._take(self.lib.pulumi_list_stacks(self._b(request_json), err))
        self._check(err)
        return s or "[]"


_lock = threading.Lock()
_cached: Optional[Native] = None


def native() -> Native:
    """Load (once) and return the native bindings."""
    global _cached
    with _lock:
        if _cached is None:
            _cached = Native(library_path())
        return _cached
