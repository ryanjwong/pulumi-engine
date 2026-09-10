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

"""Offline: no providers, no network. Typed errors, config, tags, history,
listing, export/import and the event envelope on a program-less stack."""

from __future__ import annotations

import re

import pytest

from pulumi_engine import (
    InvalidSpecError,
    Secret,
    StackNotFoundError,
    list_stacks,
    open_stack,
    version,
)

from .conftest import types


def test_version() -> None:
    assert re.match(r"pulumi-engine .* \(pulumi ", version())


def test_invalid_spec_is_typed() -> None:
    with pytest.raises(InvalidSpecError) as info:
        open_stack({"name": "dev", "project": {"name": "x"}, "backend": {"url": ""}})
    assert info.value.field == "backend.url"
    assert info.value.kind == "invalidSpec"


def test_offline_lifecycle(tmp) -> None:
    state = tmp("state")
    with open_stack(
        {
            "name": "dev",
            "project": {"name": "py-offline", "dir": tmp("proj")},
            "backend": {"url": "file://" + state},
            "secrets": {"provider": "b64"},
            "config": {"greeting": "hi", "token": Secret("s3cret")},
            "create": True,
        }
    ) as stack:
        assert stack.get_config("token") == {"value": "s3cret", "secret": True}
        assert stack.get_config("missing") is None
        stack.set_config("greeting", "hello")
        assert stack.get_config("greeting") == {"value": "hello"}

        with stack.refresh() as op:
            events = op.events()
            res = op.result()
        assert res["kind"] == "refresh"
        # The stream ends with the cancel terminator after the summary and
        # is numbered from 0.
        assert types(events)[-1] == "cancel"
        assert types(events)[-2] == "summary"
        assert [e["sequence"] for e in events] == list(range(len(events)))
        assert all(e["timestamp"] > 0 for e in events)

        stack.set_tags({"owner": "python", "env": "test"})
        assert stack.get_tags() == {"owner": "python", "env": "test"}
        stack.set_tags({"owner": "python"})
        assert stack.get_tags() == {"owner": "python"}

        history = stack.history()
        assert history[0]["kind"] == "refresh"
        assert history[0]["result"] == "succeeded"
        assert history[0]["startTime"] > 0 and history[0]["endTime"] >= history[0]["startTime"]
        assert len(stack.history(limit=1, page=1)) == 1

        listed = list_stacks({"url": "file://" + state}, {"project": "py-offline"})
        assert [s["name"] for s in listed] == ["dev"]
        assert listed[0]["fullName"].endswith("/py-offline/dev")

        exported = stack.export()
        assert exported["version"] == 3
        stack.import_(exported)
        with pytest.raises(InvalidSpecError):
            stack.import_({"nope": 1})

        # preview/up need a program; plan options are checked per kind.
        with pytest.raises(InvalidSpecError):
            stack.preview(None)  # type: ignore[arg-type]
        with pytest.raises(InvalidSpecError) as info:
            stack.refresh(options={"plan": "x.json"}).result()
        assert info.value.field == "options.plan"

        stack.remove(force=True)
    with pytest.raises(InvalidSpecError):
        stack.outputs()


def test_stack_not_found_without_create(tmp) -> None:
    with pytest.raises(StackNotFoundError) as info:
        open_stack(
            {
                "name": "ghost",
                "project": {"name": "py-offline"},
                "backend": {"url": "file://" + tmp("state")},
                "secrets": {"provider": "b64"},
            }
        )
    assert info.value.name == "ghost"
