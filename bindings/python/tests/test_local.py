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

"""Local YAML programs through the stock language host: lifecycle, typed
resource failure, cancellation (and the GIL staying free while blocked),
update plans."""

from __future__ import annotations

import json
import threading
import time
from pathlib import Path

import pytest

from pulumi_engine import (
    CancelledError,
    LocalProgram,
    Options,
    PlanViolationError,
    ResourceOpFailedError,
    open_stack,
)

from .conftest import YAML_LANGUAGE_VERSION, local, types


def test_local_yaml_preview_up_destroy(copy_fixture, spec) -> None:
    dir = copy_fixture("yaml-random")
    stack = open_stack(spec("yaml", dir, config={"petLength": "2"}))
    try:
        assert (Path(dir) / "Pulumi.yaml.yaml").exists()
        program = LocalProgram(dir, language_version=YAML_LANGUAGE_VERSION)

        with stack.preview(program) as op:
            events = op.events()
            res = op.result()
        assert types(events)[-1] == "cancel"
        assert res["kind"] == "preview"
        assert res["changes"]["create"] == 3

        with stack.up(program, Options(message="first")) as op:
            res = op.result()
        assert res["outputs"]["values"]["token"] == "[secret]"
        assert res["outputs"]["values"]["petName"].count("-") == 1
        assert stack.outputs(show_secrets=True)["values"]["token"] != "[secret]"
        assert stack.history()[0]["message"] == "first"

        with stack.destroy() as op:
            res = op.result()
        assert res["changes"]["delete"] == 3
    finally:
        stack.remove(force=True)
        stack.close()


def test_failing_resource_is_typed(copy_fixture, spec) -> None:
    dir = copy_fixture("yaml-fail")
    stack = open_stack(spec("yamlfail", dir))
    try:
        op = stack.up(local(dir))
        events = op.events()
        with pytest.raises(ResourceOpFailedError) as info:
            op.result()
        op.release()
        e = info.value
        assert e.urn.endswith("::boom")
        assert e.type == "command:local:Command"
        assert e.op == "create"
        assert "exit status 3" in e.message or "meant to fail" in e.message
        assert e.result is not None and len(e.result["failures"]) == 1
        assert "resourceOpFailed" in types(events)

        with stack.destroy() as op:
            op.result()
    finally:
        stack.remove(force=True)
        stack.close()


def test_cancel_from_another_thread_while_blocked(copy_fixture, spec) -> None:
    dir = copy_fixture("yaml-slow")
    stack = open_stack(spec("cancel", dir))
    try:
        op = stack.up(local(dir))

        # Cancel once the first slow create has started, from a thread that
        # reads events while the main thread is blocked in result(). This is
        # also the GIL check: result() is one foreign call (pulumi_op_wait)
        # entered before any event exists; if cffi did not release the GIL
        # around it, the watcher could neither read events nor cancel, and
        # the up would run its three 6 s creates to completion instead of
        # ending in CancelledError. A ticker thread counts as well, so the
        # failure message says how much Python ran meanwhile.
        def watch() -> None:
            for e in op:
                pre = e.get("resourcePreEvent")
                if pre and pre["metadata"]["urn"].endswith("::slow1"):
                    op.cancel()

        ticks = 0
        stop = threading.Event()

        def tick() -> None:
            nonlocal ticks
            while not stop.is_set():
                ticks += 1
                time.sleep(0.01)

        watcher = threading.Thread(target=watch, daemon=True)
        ticker = threading.Thread(target=tick, daemon=True)
        started = time.monotonic()
        watcher.start()
        ticker.start()
        with pytest.raises(CancelledError) as info:
            op.result()
        elapsed = time.monotonic() - started
        stop.set()
        ticker.join()
        watcher.join(timeout=10)
        op.release()

        assert elapsed < 15, f"result() blocked {elapsed:.1f}s: the watcher's cancel never took effect ({ticks} ticks)"
        assert ticks > 0, "the ticker thread never ran while result() blocked"
        assert info.value.result is not None and info.value.result["cancelled"] is True
        assert info.value.operation == "up"

        dep = stack.export()["deployment"]
        assert dep.get("pending_operations") in (None, [])
        urns = [r["urn"] for r in dep["resources"]]
        assert any(u.endswith("::first") for u in urns)
        assert not any(u.endswith("::slow3") for u in urns)

        with stack.destroy() as op:
            op.result()
    finally:
        stack.remove(force=True)
        stack.close()


def test_context_manager_cancels_on_early_exit(copy_fixture, spec) -> None:
    dir = copy_fixture("yaml-slow")
    stack = open_stack(spec("ctxcancel", dir))
    try:
        with stack.up(local(dir)) as op:
            for e in op:
                pre = e.get("resourcePreEvent")
                if pre and pre["metadata"]["urn"].endswith("::slow1"):
                    break
        # Leaving the block cancelled, waited and released.
        assert op.done
        with pytest.raises(CancelledError):
            op.result()
        with stack.destroy() as op:
            op.result()
    finally:
        stack.remove(force=True)
        stack.close()


def test_update_plans(copy_fixture, spec, tmp) -> None:
    dir = copy_fixture("yaml-random")
    stack = open_stack(spec("plan", dir, config={"petLength": "2"}))
    try:
        plan_file = Path(tmp("plan")) / "plan.json"
        with stack.preview(local(dir), {"savePlan": str(plan_file), "generatePlan": True}) as op:
            res = op.result()
        assert plan_file.is_file()
        assert res["plan"] == json.loads(plan_file.read_text())
        assert "resourcePlans" in res["plan"]

        with stack.up(local(dir), Options(plan=str(plan_file))) as op:
            res = op.result()
        assert res["changes"]["create"] == 3

        # A fresh plan proposes "same" everywhere; changing a replace-triggering
        # input exceeds it.
        with stack.preview(local(dir), Options(generate_plan=True)) as op:
            same_plan = op.result()["plan"]
        stack.set_config("petLength", "3")
        op = stack.up(local(dir), {"planJson": same_plan})
        with pytest.raises(PlanViolationError) as info:
            op.result()
        op.release()
        assert info.value.kind == "planViolation"
        assert info.value.resources[0]["urn"].endswith("::pet")
        assert "plan" in info.value.resources[0]["message"]
        assert stack.outputs()["values"]["petName"].count("-") == 1

        with stack.destroy() as op:
            op.result()
    finally:
        stack.remove(force=True)
        stack.close()
