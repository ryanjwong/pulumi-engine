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

// Inline programs: the JavaScript function runs in this process through
// @pulumi/pulumi's LanguageServer; only the provider is a child process.

import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { test } from "node:test";

import * as pulumi from "@pulumi/pulumi";
import * as random from "@pulumi/random";

import { ProgramFailedError, ResourceOpFailedError, Secret, openStack } from "../src";

function tmp(prefix: string): string {
    return fs.mkdtempSync(path.join(os.tmpdir(), `pulumi-engine-${prefix}-`));
}

test("inline program: up, outputs, destroy in one process", async () => {
    const stack = await openStack({
        name: "dev",
        project: { name: "node-inline", dir: tmp("proj") },
        backend: { url: "file://" + tmp("state") },
        secrets: { provider: "passphrase", passphrase: "pw" },
        config: { petLength: "3", apiKey: new Secret("hunter2") },
        create: true,
    });
    const pid = process.pid;
    const program = async () => {
        const cfg = new pulumi.Config();
        const pet = new random.RandomPet("pet", { length: cfg.requireNumber("petLength") });
        const pw = new random.RandomPassword("pw", { length: 16 });
        return {
            petName: pet.id,
            password: pw.result,
            apiKeyLen: cfg.requireSecret("apiKey").apply((k) => k.length),
            samePid: process.pid === pid,
        };
    };

    const preview = await stack.preview(program);
    await preview.events();
    const previewResult = await preview.result();
    await preview.release();
    assert.equal(previewResult.changes.create, 3);

    const up = await stack.up(program);
    const events = await up.events();
    const result = await up.result();
    await up.release();
    assert.equal(result.changes.create, 3);
    // The library ends every stream with the cancel terminator after the summary.
    assert.equal(events.at(-1)?.type, "cancel");
    assert.equal(events.at(-2)?.type, "summary");
    assert.equal(result.outputs?.values.samePid, true, "program must run in this process");
    assert.equal(result.outputs?.values.password, "[secret]");
    assert.equal(result.outputs?.values.apiKeyLen, "[secret]");
    assert.match(String(result.outputs?.values.petName), /^[a-z]+-[a-z]+-[a-z]+$/);
    const shown = stack.outputs(true);
    assert.equal(String(shown.values.password).length, 16);
    assert.equal(shown.values.apiKeyLen, 7);

    // second up is a no-op
    const again = await stack.up(program);
    await again.events();
    const againResult = await again.result();
    await again.release();
    assert.equal(againResult.changes.same, 3);

    const destroy = await stack.destroy();
    await destroy.events();
    const destroyResult = await destroy.result();
    await destroy.release();
    assert.equal(destroyResult.changes.delete, 3);
    stack.remove();
    stack.close();
});

test("inline program errors are typed", async () => {
    const stack = await openStack({
        name: "dev",
        project: { name: "node-inline-err", dir: tmp("proj") },
        backend: { url: "file://" + tmp("state") },
        secrets: { provider: "b64" },
        create: true,
    });
    const failing = await stack.up(async () => {
        throw new Error("kaboom from inline");
    });
    await failing.events();
    await assert.rejects(failing.result(), (e: unknown) => e instanceof ProgramFailedError && /kaboom/.test(e.message));
    await failing.release();

    const badResource = await stack.up(async () => {
        new random.RandomInteger("bad", { min: 10, max: 1 });
    });
    await badResource.events();
    await assert.rejects(
        badResource.result(),
        (e: unknown) => e instanceof ResourceOpFailedError && /::bad$/.test(e.urn) && e.op === "create",
    );
    await badResource.release();

    const destroy = await stack.destroy();
    await destroy.result();
    await destroy.release();
    stack.remove(true);
    stack.close();
});
