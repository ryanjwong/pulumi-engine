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

"""Stacks and operations over the C ABI.

The engine runs inside this process (libpulumi, loaded through cffi);
provider plugins are child processes as the Pulumi protocol requires. No
``pulumi`` CLI is involved.
"""

from __future__ import annotations

import json
import threading
from dataclasses import asdict, dataclass, fields, is_dataclass
from typing import Any, Callable, Iterator, Mapping, Optional, Union

from ._inline import InlineServer, start_inline_server
from ._native import native
from .errors import InvalidSpecError, PulumiError

__all__ = [
    "Secret",
    "LocalProgram",
    "CallbackProgram",
    "Options",
    "Operation",
    "Stack",
    "open_stack",
    "list_stacks",
    "version",
]

Event = dict[str, Any]
Result = dict[str, Any]
Program = Union["LocalProgram", "CallbackProgram", Mapping[str, Any], Callable[[], Any]]


class Secret:
    """Wrap a config value to mark it secret."""

    __slots__ = ("value",)

    def __init__(self, value: str) -> None:
        self.value = value

    def __repr__(self) -> str:
        return "Secret([secret])"


@dataclass
class LocalProgram:
    """A project directory run by its language host. ``language_version``
    pins the runtime version the host should use ("1.38.5"), when the
    runtime supports it."""

    dir: str
    language_version: Optional[str] = None

    def to_wire(self) -> dict[str, Any]:
        wire: dict[str, Any] = {"mode": "local", "dir": self.dir}
        if self.language_version:
            wire["languageVersion"] = self.language_version
        return wire


@dataclass
class CallbackProgram:
    """A LanguageRuntime gRPC server the caller runs (``pulumi up --client``)."""

    address: str

    def to_wire(self) -> dict[str, Any]:
        return {"mode": "callback", "address": self.address}


# Option field name -> wire (camelCase) key. Dict options may use either.
_OPTION_KEYS = {
    "parallel": "parallel",
    "message": "message",
    "targets": "targets",
    "target_dependents": "targetDependents",
    "excludes": "excludes",
    "replaces": "replaces",
    "refresh": "refresh",
    "continue_on_error": "continueOnError",
    "show_secrets": "showSecrets",
    "dry_run": "dryRun",
    "env": "env",
    "save_plan": "savePlan",
    "generate_plan": "generatePlan",
    "plan": "plan",
    "plan_json": "planJson",
}
_WIRE_KEYS = set(_OPTION_KEYS.values())


@dataclass
class Options:
    """Options for one operation (the zero value is a plain operation).

    Plans: ``save_plan`` (preview; a path) and ``generate_plan`` (preview;
    the plan is returned in ``result()["plan"]``) generate the CLI's plan
    file; ``plan`` (up; a path) or ``plan_json`` (up; the plan document, a
    dict or JSON string) constrain the update to it, failing with
    :class:`~pulumi_engine.errors.PlanViolationError` otherwise.
    """

    parallel: Optional[int] = None
    message: Optional[str] = None
    targets: Optional[list[str]] = None
    target_dependents: bool = False
    excludes: Optional[list[str]] = None
    replaces: Optional[list[str]] = None
    refresh: bool = False
    continue_on_error: bool = False
    show_secrets: bool = False
    dry_run: bool = False
    env: Optional[dict[str, str]] = None
    save_plan: Optional[str] = None
    generate_plan: bool = False
    plan: Optional[str] = None
    plan_json: Optional[Union[dict[str, Any], str, bytes]] = None

    def to_wire(self) -> dict[str, Any]:
        return _options_to_wire(asdict(self))


def _options_to_wire(options: Optional[Mapping[str, Any]]) -> dict[str, Any]:
    """Normalise an options mapping (snake_case or wire keys) to wire keys,
    dropping unset values."""
    wire: dict[str, Any] = {}
    for key, value in (options or {}).items():
        if value is None or value is False:
            continue
        if key in _OPTION_KEYS:
            key = _OPTION_KEYS[key]
        elif key not in _WIRE_KEYS:
            raise InvalidSpecError("options", f"unknown option {key!r}")
        if key == "planJson":
            if isinstance(value, (bytes, bytearray)):
                value = value.decode("utf-8")
            if isinstance(value, str):
                value = json.loads(value)
        wire[key] = value
    return wire


def _config_to_wire(config: Optional[Mapping[str, Any]]) -> Optional[dict[str, Any]]:
    if config is None:
        return None
    out: dict[str, Any] = {}
    for key, value in config.items():
        if isinstance(value, Secret):
            out[key] = {"value": value.value, "secret": True}
        elif isinstance(value, str):
            out[key] = {"value": value}
        elif isinstance(value, Mapping):
            out[key] = dict(value)
        else:
            raise InvalidSpecError(f"config.{key}", "config values are strings, Secret, or {value, secret, object} dicts")
    return out


def _spec_to_wire(spec: Any) -> dict[str, Any]:
    if is_dataclass(spec) and not isinstance(spec, type):
        spec = {f.name: getattr(spec, f.name) for f in fields(spec)}
    if not isinstance(spec, Mapping):
        raise InvalidSpecError("spec", "expected a dict (or dataclass) describing the stack")
    wire = dict(spec)
    if "config" in wire:
        wire["config"] = _config_to_wire(wire["config"])
    return wire


def _program_to_wire(program: Optional[Program]) -> tuple[Optional[dict[str, Any]], Optional[InlineServer]]:
    """The wire form of a program and, for inline programs, the server
    hosting it (to be closed after the operation)."""
    if program is None:
        return None, None
    if isinstance(program, (LocalProgram, CallbackProgram)):
        return program.to_wire(), None
    if isinstance(program, Mapping):
        return dict(program), None
    if callable(program):
        server = start_inline_server(program)
        return {"mode": "callback", "address": server.address}, server
    raise InvalidSpecError("program", "expected LocalProgram, CallbackProgram, a program dict or a callable")


class Operation:
    """A running or finished preview, up, refresh or destroy.

    Iterate it for events (dicts in Pulumi's engine event JSON form plus a
    ``type`` field) and call :meth:`result` for the outcome; both may be used
    from different threads, neither is required. Blocking calls release the
    GIL. As a context manager, leaving the block cancels an operation that is
    still running, waits for it and releases the native handle.
    """

    _POLL_MS = 250

    def __init__(self, kind: str, op_id: int, cleanup: Optional[Callable[[], None]] = None) -> None:
        self.kind = kind
        self._id = op_id
        self._cleanup = cleanup
        self._lock = threading.Lock()
        self._result: Optional[Result] = None
        self._error: Optional[PulumiError] = None
        self._waited = False
        self._released = False

    # -- events ----------------------------------------------------------

    def __iter__(self) -> Iterator[Event]:
        n = native()
        while True:
            text = n.op_next_event(self._id, self._POLL_MS)
            if text is None:
                return
            if text == "":
                continue  # timeout: lets KeyboardInterrupt through
            yield json.loads(text)

    def events(self) -> list[Event]:
        """Collect every event into a list."""
        return list(self)

    # -- outcome ---------------------------------------------------------

    def result(self) -> Result:
        """Block until the operation finishes; return the result dict or
        raise the typed error (which carries the partial result)."""
        with self._lock:
            if not self._waited:
                try:
                    self._result = json.loads(native().op_wait(self._id))
                except PulumiError as exn:
                    self._error = exn
                finally:
                    self._waited = True
                    if self._cleanup is not None:
                        cleanup, self._cleanup = self._cleanup, None
                        cleanup()
            if self._error is not None:
                raise self._error
            assert self._result is not None
            return self._result

    def wait(self) -> tuple[Optional[Result], Optional[PulumiError]]:
        """Like :meth:`result` but returns ``(result, error)`` instead of raising."""
        try:
            return self.result(), None
        except PulumiError as exn:
            return exn.result, exn

    @property
    def done(self) -> bool:
        """Whether :meth:`result` has completed (cheap; does not block)."""
        return self._waited

    def cancel(self) -> None:
        """Graceful cancel; a second call terminates the engine."""
        if not self._released:
            native().op_cancel(self._id)

    def release(self) -> None:
        """Release the native handle once the operation is done with."""
        if self._released:
            return
        self.wait()
        self._released = True
        native().op_release(self._id)

    # -- context manager -------------------------------------------------

    def __enter__(self) -> "Operation":
        return self

    def __exit__(self, *_: object) -> None:
        if not self._waited:
            self.cancel()
        self.release()


class Stack:
    """An open stack. Operations from one handle are serialised by the
    library; the second concurrent one fails with ConcurrentUpdateError."""

    def __init__(self, handle: int, name: str) -> None:
        self._handle = handle
        self.name = name
        self._closed = False

    def _ensure_open(self) -> int:
        if self._closed:
            raise InvalidSpecError("stack", "stack handle is closed")
        return self._handle

    def _start(self, kind: str, program: Optional[Program], options: Optional[Union[Options, Mapping[str, Any]]]) -> Operation:
        handle = self._ensure_open()
        wire_program, server = _program_to_wire(program)
        if kind in ("preview", "up") and wire_program is None:
            raise InvalidSpecError("program", f"{kind} requires a program")
        wire_options = options.to_wire() if isinstance(options, Options) else _options_to_wire(options)
        request = {"kind": kind, "options": wire_options}
        if wire_program is not None:
            request["program"] = wire_program
        try:
            op_id = native().op_start(handle, json.dumps(request))
        except PulumiError:
            if server is not None:
                server.close()
            raise
        return Operation(kind, op_id, server.close if server is not None else None)

    def preview(self, program: Program, options: Optional[Union[Options, Mapping[str, Any]]] = None) -> Operation:
        """Compute the changes an up would make without applying them."""
        return self._start("preview", program, options)

    def up(self, program: Program, options: Optional[Union[Options, Mapping[str, Any]]] = None) -> Operation:
        """Run the program and apply the resulting changes."""
        return self._start("up", program, options)

    def refresh(self, program: Optional[Program] = None, options: Optional[Union[Options, Mapping[str, Any]]] = None) -> Operation:
        """Reconcile the checkpoint with the providers' view of the world."""
        return self._start("refresh", program, options)

    def destroy(self, program: Optional[Program] = None, options: Optional[Union[Options, Mapping[str, Any]]] = None) -> Operation:
        """Delete every resource in the stack."""
        return self._start("destroy", program, options)

    def outputs(self, show_secrets: bool = False) -> dict[str, Any]:
        """Stack outputs as ``{"values": {...}, "secretKeys": [...]}``;
        secrets redacted unless ``show_secrets``."""
        return json.loads(native().stack_outputs(self._ensure_open(), show_secrets))

    def export(self) -> dict[str, Any]:
        """The checkpoint as Pulumi's untyped deployment ``{version, deployment}``."""
        return json.loads(native().stack_export(self._ensure_open()))

    def import_(self, deployment: Any) -> None:
        """Replace the checkpoint."""
        native().stack_import(self._ensure_open(), json.dumps(deployment))

    def set_config(self, key: str, value: Union[str, Secret, Mapping[str, Any]]) -> None:
        wire = _config_to_wire({key: value})
        assert wire is not None
        native().stack_set_config(self._ensure_open(), key, json.dumps(wire[key]))

    def get_config(self, key: str) -> Optional[dict[str, Any]]:
        """``{"value": ..., "secret": bool, "object": bool}`` or None when unset."""
        v = json.loads(native().stack_get_config(self._ensure_open(), key))
        return None if v is None else v

    def get_tags(self) -> dict[str, str]:
        return json.loads(native().stack_get_tags(self._ensure_open()))

    def set_tags(self, tags: Mapping[str, str]) -> None:
        """Replace every tag on the stack."""
        native().stack_set_tags(self._ensure_open(), json.dumps(dict(tags)))

    def history(self, limit: Optional[int] = None, page: Optional[int] = None, show_secrets: bool = False) -> list[dict[str, Any]]:
        """The stack's update history, newest first. Previews are not recorded."""
        opts: dict[str, Any] = {}
        if limit:
            opts["limit"] = limit
        if page:
            opts["page"] = page
        if show_secrets:
            opts["showSecrets"] = True
        return json.loads(native().stack_history(self._ensure_open(), json.dumps(opts)))

    def cancel(self) -> None:
        """Cancel running operations (and the backend's current update where supported)."""
        native().stack_cancel(self._ensure_open())

    def remove(self, force: bool = False) -> None:
        """Delete the stack from the backend."""
        native().stack_remove(self._ensure_open(), force)

    def close(self) -> None:
        """Release the native handle."""
        if not self._closed:
            self._closed = True
            native().stack_close(self._handle)

    def __enter__(self) -> "Stack":
        return self

    def __exit__(self, *_: object) -> None:
        self.close()


def open_stack(spec: Union[Mapping[str, Any], Any]) -> Stack:
    """Open (or with ``create: True``, create) a stack from a spec dict
    (keys as in the Node binding's ``StackSpec``: name, project, backend,
    secrets, config, env, create)."""
    wire = _spec_to_wire(spec)
    handle = native().stack_open(json.dumps(wire))
    return Stack(handle, str(wire.get("name", "")))


def list_stacks(backend: Mapping[str, Any], filter: Optional[Mapping[str, Any]] = None) -> list[dict[str, Any]]:  # noqa: A002
    """List the stacks a backend holds (``{url, token?}``; filter keys
    project, organization, tagName, tagValue)."""
    return json.loads(native().list_stacks(json.dumps({"backend": dict(backend), "filter": dict(filter or {})})))


def version() -> str:
    """Version of the library and of the Pulumi packages it embeds."""
    return native().version()
