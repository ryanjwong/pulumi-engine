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

"""Inline programs: run a Python callable as the Pulumi program in this
process.

This uses the same ``LanguageServer`` class the Python Automation API uses
for inline programs (``pulumi.automation._server``), served on a loopback
gRPC port that the engine connects to as a "callback" program. ``pulumi``
and ``grpcio`` are optional dependencies (the ``inline`` extra); they are
imported only when an inline program is used.
"""

from __future__ import annotations

import threading
import traceback
from concurrent.futures import Future
from typing import Any, Callable, Optional

from .errors import InvalidSpecError

__all__ = ["InlineServer", "start_inline_server"]


class _DaemonExecutor:
    """A minimal executor for the gRPC server whose workers are daemon
    threads.

    After a failed deployment Pulumi's engine does not shut down its resource
    monitor (``deploymentExecutor.Execute`` only cancels the source iterator
    on success; the CLI never notices because it exits). The SDK's
    ``SignalAndWaitForShutdown`` call inside the ``Run`` handler then never
    returns. With ``concurrent.futures.ThreadPoolExecutor`` that stuck worker
    would be joined at interpreter exit and hang the process; a daemon
    thread is simply abandoned.
    """

    def __init__(self) -> None:
        self._threads: list[threading.Thread] = []
        self._lock = threading.Lock()

    def submit(self, fn: Callable[..., Any], /, *args: Any, **kwargs: Any) -> Future:
        fut: Future = Future()

        def run() -> None:
            if not fut.set_running_or_notify_cancel():
                return
            try:
                fut.set_result(fn(*args, **kwargs))
            except BaseException as exn:  # noqa: BLE001 - propagate through the future
                fut.set_exception(exn)

        t = threading.Thread(target=run, name="pulumi-engine-inline", daemon=True)
        with self._lock:
            self._threads = [x for x in self._threads if x.is_alive()]
            self._threads.append(t)
        t.start()
        return fut

    def shutdown(self, wait: bool = True, *, cancel_futures: bool = False) -> None:
        """Threads are daemons; nothing to join. Matches the Executor API."""


def _load() -> tuple[Any, Any, Any, Any, Any]:
    try:
        import grpc
        import pulumi
        from pulumi.automation._server import LanguageServer
        from pulumi.runtime.proto import language_pb2_grpc
    except ImportError as exn:  # pragma: no cover - depends on the environment
        raise InvalidSpecError(
            "program",
            f"inline programs need the 'pulumi' and 'grpcio' packages installed "
            f"(pip install 'pulumi_engine[inline]'): {exn}",
        ) from exn
    try:
        from pulumi.runtime._grpc_settings import _GRPC_CHANNEL_OPTIONS as channel_options
    except ImportError:  # older SDKs: defaults are fine
        channel_options = None
    return grpc, pulumi, LanguageServer, language_pb2_grpc, channel_options


class InlineServer:
    """A running LanguageRuntime server hosting one inline program."""

    def __init__(self, address: str, server: Any) -> None:
        self.address = address
        self._server = server

    def close(self) -> None:
        """Stop the server; a handler still blocked on the monitor is
        abandoned on its daemon thread (see :class:`_DaemonExecutor`)."""
        self._server.stop(0)


def start_inline_server(program: Callable[[], Any]) -> InlineServer:
    """Serve ``program`` as a Pulumi LanguageRuntime on a loopback port."""
    grpc, pulumi, LanguageServer, language_pb2_grpc, channel_options = _load()

    def wrapped() -> Optional[Any]:
        try:
            return program()
        except pulumi.RunError:
            raise
        except Exception as exn:  # noqa: BLE001 - re-reported as a RunError
            # A RunError makes the SDK's LanguageServer answer the engine's
            # Run call with an error (and log it), which fails the operation
            # as ProgramFailedError with this message. Any other exception is
            # reported the same way but wrapped in the SDK's own "python
            # inline source runtime error" text; the traceback is kept so
            # the caller can see where the program failed.
            trace = "".join(traceback.format_exception(type(exn), exn, exn.__traceback__))
            raise pulumi.RunError(f"program failed: {exn}\n{trace}") from exn

    server = grpc.server(_DaemonExecutor(), options=channel_options) if channel_options else grpc.server(_DaemonExecutor())
    language_pb2_grpc.add_LanguageRuntimeServicer_to_server(LanguageServer(wrapped), server)
    port = server.add_insecure_port("127.0.0.1:0")
    if not port:
        raise InvalidSpecError("program", "could not bind a loopback port for the inline program server")
    server.start()
    return InlineServer(f"127.0.0.1:{port}", server)
