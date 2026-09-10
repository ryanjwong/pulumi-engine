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

"""Typed exceptions, built from the C ABI's error JSON.

Every error the library raises is a :class:`PulumiError` whose ``kind``
mirrors ``engine.ErrorKind`` in Go; the subclasses carry the kind-specific
fields (``urn``/``op`` for a failed resource, ``resources`` for a plan
violation, ...). ``result`` holds the partial operation result when the
error came from a finished operation.
"""

from __future__ import annotations

import json
from typing import Any, Optional

__all__ = [
    "PulumiError",
    "InvalidSpecError",
    "ResourceOpFailedError",
    "ProgramFailedError",
    "ConcurrentUpdateError",
    "StackNotFoundError",
    "StackExistsError",
    "PendingOperationsError",
    "CancelledError",
    "PlanViolationError",
    "UnsupportedError",
    "error_from_json",
]


class PulumiError(Exception):
    """Base class of every error raised by the library."""

    kind: str
    result: Optional[dict[str, Any]]

    def __init__(self, kind: str, message: str, result: Optional[dict[str, Any]] = None) -> None:
        super().__init__(message)
        self.kind = kind
        self.result = result

    @property
    def message(self) -> str:
        """The error message."""
        return str(self.args[0]) if self.args else ""


class InvalidSpecError(PulumiError):
    """A StackSpec or Options problem detected before any engine work."""

    def __init__(self, field: str, message: str) -> None:
        super().__init__("invalidSpec", f"{field}: {message}")
        self.field = field


class ResourceOpFailedError(PulumiError):
    """A resource operation failed; ``result["failures"]`` has all of them."""

    def __init__(
        self,
        urn: str,
        type: str,  # noqa: A002 - the ABI field name
        op: str,
        provider: str,
        message: str,
        result: Optional[dict[str, Any]] = None,
    ) -> None:
        super().__init__("resourceOpFailed", f"{op} {type} ({urn}): {message}", result)
        self.urn = urn
        self.type = type
        self.op = op
        self.provider = provider


class ProgramFailedError(PulumiError):
    """The Pulumi program itself failed (exception, non-zero exit, failed invoke)."""

    def __init__(self, message: str, result: Optional[dict[str, Any]] = None) -> None:
        super().__init__("programFailed", message, result)


class ConcurrentUpdateError(PulumiError):
    """Another update holds the stack's lock."""

    def __init__(self, message: str, result: Optional[dict[str, Any]] = None) -> None:
        super().__init__("concurrentUpdate", message, result)


class StackNotFoundError(PulumiError):
    """The stack does not exist and ``create`` was not set."""

    def __init__(self, name: str, message: str) -> None:
        super().__init__("stackNotFound", message)
        self.name = name


class StackExistsError(PulumiError):
    """Stack creation raced with another creator."""

    def __init__(self, name: str, message: str) -> None:
        super().__init__("stackExists", message)
        self.name = name


class PendingOperationsError(PulumiError):
    """The checkpoint records interrupted operations that must be resolved first."""

    def __init__(self, urns: list[str], message: str, result: Optional[dict[str, Any]] = None) -> None:
        super().__init__("pendingOperations", message, result)
        self.urns = urns


class CancelledError(PulumiError):
    """The operation was cancelled; the checkpoint reflects every completed step."""

    def __init__(self, operation: str, message: str, result: Optional[dict[str, Any]] = None) -> None:
        super().__init__("cancelled", message, result)
        self.operation = operation


class PlanViolationError(PulumiError):
    """An up constrained by a plan tried operations the plan did not propose.

    ``resources`` is a list of ``{"urn": ..., "message": ...}`` dicts, one per
    violation the engine reported.
    """

    def __init__(
        self, resources: list[dict[str, str]], message: str, result: Optional[dict[str, Any]] = None
    ) -> None:
        super().__init__("planViolation", message, result)
        self.resources = resources


class UnsupportedError(PulumiError):
    """The backend (or Pulumi at the pinned version) does not support the feature."""

    def __init__(self, feature: str, backend: str, message: str) -> None:
        super().__init__("unsupported", message)
        self.feature = feature
        self.backend = backend


def error_from_json(text: str) -> PulumiError:
    """Build the right exception class from the ABI's error JSON."""
    try:
        e = json.loads(text)
    except ValueError:
        return PulumiError("unclassified", text)
    if not isinstance(e, dict):
        return PulumiError("unclassified", text)
    message = str(e.get("message", "unknown error"))
    result = e.get("result")
    kind = e.get("kind")
    if kind == "invalidSpec":
        return InvalidSpecError(str(e.get("field", "")), message)
    if kind == "resourceOpFailed":
        return ResourceOpFailedError(
            str(e.get("urn", "")),
            str(e.get("type", "")),
            str(e.get("op", "")),
            str(e.get("provider", "")),
            message,
            result,
        )
    if kind == "programFailed":
        return ProgramFailedError(message, result)
    if kind == "concurrentUpdate":
        return ConcurrentUpdateError(message, result)
    if kind == "stackNotFound":
        return StackNotFoundError(str(e.get("name", "")), message)
    if kind == "stackExists":
        return StackExistsError(str(e.get("name", "")), message)
    if kind == "pendingOperations":
        return PendingOperationsError(list(e.get("urns") or []), message, result)
    if kind == "cancelled":
        return CancelledError(str(e.get("operation", "")), message, result)
    if kind == "planViolation":
        return PlanViolationError(list(e.get("resources") or []), message, result)
    if kind == "unsupported":
        return UnsupportedError(str(e.get("feature", "")), str(e.get("backend", "")), message)
    return PulumiError(str(kind or "unclassified"), message, result)
