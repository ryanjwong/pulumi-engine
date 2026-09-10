// Copyright 2026 Ryan Wong
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Tests against the built libpulumi. The "local" tests run the YAML language
// host and the random/command providers (downloaded on first use).

import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { test } from "node:test";

import {
    CancelledError,
    InvalidSpecError,
    ResourceOpFailedError,
    Secret,
    StackNotFoundError,
    openStack,
    version,
    type Event,
} from "../src";

const fixtures = path.join(__dirname, "..", "..", "..", "..", "engine", "testdata");

function tmp(prefix: string): string {
    return fs.mkdtempSync(path.join(os.tmpdir(), `pulumi-engine-${prefix}-`));
}

function copyFixture(name: string): string {
    const dir = tmp(name);
    for (const f of fs.readdirSync(path.join(fixtures, name))) {
        fs.copyFileSync(path.join(fixtures, name, f), path.join(dir, f));
    }
    return dir;
}

function types(events: Event[]): string[] {
    return events.map((e) => e.type);
}

test("version", () => {
    assert.match(version(), /pulumi-engine .* \(pulumi v3\./);
});

test("invalid spec is a typed error", async () => {
    await assert.rejects(
        openStack({ name: "dev", project: { name: "x" }, backend: { url: "" } }),
        (e: unknown) => e instanceof InvalidSpecError && e.field === "backend.url" && e.kind === "invalidSpec",
    );
});

test("offline lifecycle with a program-less stack", async () => {
    const stack = await openStack({
        name: "dev",
        project: { name: "node-offline", dir: tmp("proj") },
        backend: { url: "file://" + tmp("state") },
        secrets: { provider: "b64" },
        config: { greeting: "hi", token: new Secret("s3cret") },
        create: true,
    });
    assert.deepEqual(stack.getConfig("token"), { value: "s3cret", secret: true });
    assert.equal(stack.getConfig("missing"), undefined);
    stack.setConfig("greeting", "hello");
    assert.deepEqual(stack.getConfig("greeting"), { value: "hello" });

    const refresh = await stack.refresh();
    const events = await refresh.events();
    const res = await refresh.result();
    await refresh.release();
    assert.equal(res.kind, "refresh");
    assert.equal(types(events).at(-1), "summary");

    const exported = stack.export() as { version: number };
    assert.equal(exported.version, 3);
    stack.import(exported);
    assert.throws(() => stack.import({ nope: 1 }), InvalidSpecError);

    await assert.rejects((await stack.up({ mode: "callback", address: "" })).result(), InvalidSpecError);
    assert.deepEqual(stack.outputs(), { values: {}, secretKeys: [] });
    stack.remove();
    stack.close();
    assert.throws(() => stack.outputs(), InvalidSpecError);
});

test("stack not found without create", async () => {
    await assert.rejects(
        openStack({
            name: "nope",
            project: { name: "p" },
            backend: { url: "file://" + tmp("state") },
            secrets: { provider: "b64" },
        }),
        StackNotFoundError,
    );
});

test("local YAML program: preview, up, destroy", async () => {
    const dir = copyFixture("yaml-random");
    const stack = await openStack({
        name: "dev",
        project: { dir },
        backend: { url: "file://" + tmp("state") },
        secrets: { provider: "passphrase", passphrase: "pw" },
        config: { petLength: "2" },
        create: true,
    });
    const program = { mode: "local" as const, dir };

    const preview = await stack.preview(program);
    const previewEvents = await preview.events();
    const previewResult = await preview.result();
    await preview.release();
    assert.ok(previewResult.summary?.isPreview);
    assert.equal(previewResult.changes.create, 3);
    assert.ok(types(previewEvents).includes("prelude"));
    assert.equal(types(previewEvents).at(-1), "summary");

    const up = await stack.up(program, { message: "from node" });
    const seen: string[] = [];
    for await (const e of up) {
        seen.push(e.type);
        if (e.resOutputsEvent?.metadata.type === "random:index/randomPassword:RandomPassword") {
            assert.match(JSON.stringify(e.resOutputsEvent.metadata.new?.outputs?.result), /\[secret\]/);
        }
    }
    const upResult = await up.result();
    await up.release();
    assert.equal(upResult.changes.create, 3);
    assert.equal(upResult.outputs?.values.token, "[secret]");
    assert.deepEqual(upResult.outputs?.secretKeys, ["token"]);
    assert.match(String(upResult.outputs?.values.petName), /^[a-z]+-[a-z]+$/);
    assert.ok(seen.filter((t) => t === "resourceOutputs").length >= 3);

    const shown = stack.outputs(true);
    assert.equal(String(shown.values.token).length, 12);

    const destroy = await stack.destroy();
    await destroy.events();
    const destroyResult = await destroy.result();
    await destroy.release();
    assert.equal(destroyResult.changes.delete, 3);
    stack.remove();
    stack.close();
});

test("failing resource is a ResourceOpFailedError with the partial result", async () => {
    const dir = copyFixture("yaml-fail");
    const stack = await openStack({
        name: "dev",
        project: { dir },
        backend: { url: "file://" + tmp("state") },
        secrets: { provider: "b64" },
        create: true,
    });
    const up = await stack.up({ mode: "local", dir });
    const events = await up.events();
    assert.ok(types(events).includes("resourceOpFailed"));
    await assert.rejects(up.result(), (e: unknown) => {
        assert.ok(e instanceof ResourceOpFailedError, `got ${e}`);
        assert.equal(e.kind, "resourceOpFailed");
        assert.match(e.urn, /::boom$/);
        assert.equal(e.op, "create");
        assert.equal(e.type, "command:local:Command");
        assert.match(e.message, /exit status 3|meant to fail/);
        const result = e.result as { failures: unknown[] };
        assert.equal(result.failures.length, 1);
        return true;
    });
    await up.release();
    const destroy = await stack.destroy();
    await destroy.result();
    await destroy.release();
    stack.remove();
    stack.close();
});

test("AbortSignal cancels an up", async () => {
    const dir = copyFixture("yaml-slow");
    const stack = await openStack({
        name: "dev",
        project: { dir },
        backend: { url: "file://" + tmp("state") },
        secrets: { provider: "b64" },
        create: true,
    });
    const ac = new AbortController();
    const up = await stack.up({ mode: "local", dir }, { signal: ac.signal });
    for await (const e of up) {
        if (e.resourcePreEvent?.metadata.urn.endsWith("::slow1")) {
            ac.abort();
        }
    }
    await assert.rejects(up.result(), (e: unknown) => e instanceof CancelledError && e.operation === "up");
    await up.release();
    // checkpoint is usable afterwards
    const destroy = await stack.destroy();
    await destroy.result();
    await destroy.release();
    stack.remove(true);
    stack.close();
});
