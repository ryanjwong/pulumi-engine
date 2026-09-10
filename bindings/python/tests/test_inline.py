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

"""Inline programs: a Python callable run in this process through the SDK's
LanguageServer, served on a loopback port the engine connects to."""

from __future__ import annotations

import os

import pytest

from pulumi_engine import ProgramFailedError, open_stack

pulumi = pytest.importorskip("pulumi")
random = pytest.importorskip("pulumi_random")


def test_inline_program_runs_in_this_process(spec) -> None:
    stack = open_stack(spec("inline", config={"petLength": "2"}))
    try:

        def program() -> None:
            length = int(pulumi.Config().require("petLength"))
            pet = random.RandomPet("pet", length=length)
            pulumi.export("petName", pet.id)
            pulumi.export("pid", os.getpid())

        with stack.up(program) as op:
            events = op.events()
            res = op.result()
        assert res["changes"]["create"] >= 2
        assert res["outputs"]["values"]["pid"] == os.getpid()
        assert res["outputs"]["values"]["petName"].count("-") == 1
        assert any(e["type"] == "resourceOutputs" for e in events)

        with stack.destroy() as op:
            assert op.result()["changes"]["delete"] >= 2
    finally:
        stack.remove(force=True)
        stack.close()


def test_inline_program_error_is_typed(spec) -> None:
    stack = open_stack(spec("inline-fail"))
    try:

        def program() -> None:
            raise RuntimeError("kaboom from the program")

        op = stack.up(program)
        with pytest.raises(ProgramFailedError) as info:
            op.result()
        op.release()
        assert "kaboom from the program" in info.value.message
    finally:
        stack.remove(force=True)
        stack.close()
