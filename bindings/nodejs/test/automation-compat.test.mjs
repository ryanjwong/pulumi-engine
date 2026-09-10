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

// The Automation API facade, driven the way an Automation API consumer
// drives the SDK: LocalWorkspace openers, option bags with callbacks,
// SDK-shaped results and errors. Runs the YAML fixtures (language host and
// providers downloaded on first use) and one inline program.

import assert from "node:assert/strict";
import * as fs from "node:fs";
import { createRequire } from "node:module";
import * as os from "node:os";
import * as path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import {
    CommandError,
    LocalWorkspace,
    PulumiCommand,
    StackAlreadyExistsError,
    StackNotFoundError,
    createAutomationModule,
    embeddedPulumiVersion,
    readProjectSettings,
} from "../dist/src/automation-compat.js";
import { CancelledError, ResourceOpFailedError } from "../dist/src/index.js";

const require = createRequire(import.meta.url);
const here = path.dirname(fileURLToPath(import.meta.url));
const fixtures = path.join(here, "..", "..", "..", "engine", "testdata");

function tmp(prefix) {
    return fs.mkdtempSync(path.join(os.tmpdir(), `automation-compat-${prefix}-`));
}

function copyFixture(name) {
    const dir = tmp(name);
    for (const f of fs.readdirSync(path.join(fixtures, name))) {
        fs.copyFileSync(path.join(fixtures, name, f), path.join(dir, f));
    }
    return dir;
}

/** The workspace options an Automation API consumer passes: env for the engine, a passphrase. */
function workspaceOptions(extra = {}) {
    return {
        envVars: { PULUMI_BACKEND_URL: "file://" + tmp("state"), PULUMI_CONFIG_PASSPHRASE: "pw" },
        secretsProvider: "passphrase",
        ...extra,
    };
}

test("PulumiCommand reports the embedded Pulumi version and never runs a CLI", async () => {
    const cmd = await PulumiCommand.get();
    assert.ok(fs.existsSync(cmd.command), `command names the library: ${cmd.command}`);
    assert.equal(cmd.version.major, 3);
    assert.equal(cmd.version.toString(), embeddedPulumiVersion());
    assert.equal(typeof cmd.version.compare, "function");
    await assert.rejects(cmd.run(["version"]), /no pulumi CLI/);
});

test("local YAML program: preview, up, outputs, history, export, destroy, remove", async () => {
    const workDir = copyFixture("yaml-random");
    const opts = workspaceOptions({ stackSettings: { dev: { config: { petLength: "2" } } } });
    const stack = await LocalWorkspace.createOrSelectStack({ stackName: "dev", workDir }, opts);
    assert.equal(stack.name, "dev");
    assert.equal(stack.workspace.workDir, workDir);
    assert.equal(stack.workspace.pulumiVersion, embeddedPulumiVersion());
    assert.deepEqual(await stack.getConfig("petLength"), { value: "2", secret: false });
    assert.equal((await stack.workspace.projectSettings()).name, "engine-yaml");

    // preview: SDK-shaped result, EngineEvents with the SDK's field names.
    const events = [];
    let output = "";
    const preview = await stack.preview({
        diff: true,
        onEvent: (e) => events.push(e),
        onOutput: (s) => (output += s),
    });
    assert.deepEqual(preview.changeSummary, { create: 3 });
    assert.equal(preview.stdout, output);
    assert.ok(events.every((e) => typeof e.sequence === "number" && !("type" in e)));
    assert.ok(events.some((e) => e.preludeEvent));
    assert.ok(events.some((e) => e.resourcePreEvent?.metadata.op === "create"));
    // The SDK's stream ends with the engine's cancelEvent marker after the summary.
    assert.deepEqual(events.at(-1).cancelEvent, {});
    assert.deepEqual(events.map((e) => e.sequence), events.map((_, i) => i));
    const summary = events.at(-2).summaryEvent;
    assert.equal(summary.isPreview, true);
    assert.deepEqual(summary.resourceChanges, { create: 3 });
    assert.match(output, /^Previewing update \(dev\):/);
    assert.match(output, /random:index\/randomPet:RandomPet pet creat/);
    assert.match(output, /\+ +3 created/);

    // up: outputs with {value, secret}, an UpdateSummary, history and info.
    const up = await stack.up({ message: "from the facade", parallel: 4, onOutput: () => undefined });
    assert.deepEqual(up.summary.resourceChanges, { create: 3 });
    assert.equal(up.summary.kind, "update");
    assert.equal(up.summary.result, "succeeded");
    assert.equal(up.summary.message, "from the facade");
    assert.ok(up.summary.startTime instanceof Date && up.summary.endTime instanceof Date);
    assert.equal(up.outputs.token.secret, true);
    assert.equal(String(up.outputs.token.value).length, 12);
    assert.equal(up.outputs.petName.secret, false);
    assert.match(String(up.outputs.petName.value), /^[a-z]+-[a-z]+$/);
    assert.deepEqual(await stack.outputs(), up.outputs);
    assert.equal((await stack.info()).version, 1);
    assert.equal((await stack.history()).length, 1);

    // state: export/import round trip and the workspace view of the stack.
    const exported = await stack.exportStack();
    assert.equal(exported.version, 3);
    await stack.importStack(exported);
    assert.deepEqual(await stack.workspace.stackOutputs("dev"), up.outputs);
    assert.deepEqual(
        (await stack.workspace.listStacks()).map((s) => s.name),
        ["dev"],
    );
    const stackSettings = await stack.workspace.stackSettings("dev");
    assert.match(stackSettings.encryptionSalt ?? "", /^v1:/);
    assert.ok(fs.existsSync(path.join(workDir, "Pulumi.dev.yaml")));

    // destroy: preview first, then for real, then remove.
    const previewDestroy = await stack.previewDestroy();
    assert.deepEqual(previewDestroy.changeSummary, { delete: 3 });
    const destroy = await stack.destroy();
    assert.deepEqual(destroy.summary.resourceChanges, { delete: 3 });
    assert.equal(destroy.summary.kind, "destroy");
    assert.equal((await stack.info()).version, 2);
    await stack.workspace.removeStack("dev");
    await assert.rejects(
        LocalWorkspace.selectStack({ stackName: "dev", workDir }, opts),
        (e) => e instanceof StackNotFoundError && /no stack named 'dev' found/.test(e.commandResult.stderr),
    );
});

test("expectNoChanges fails a changing preview the way the CLI does", async () => {
    const workDir = copyFixture("yaml-random");
    const stack = await LocalWorkspace.createOrSelectStack({ stackName: "dev", workDir }, workspaceOptions());
    let stderr = "";
    await assert.rejects(
        stack.preview({ expectNoChanges: true, onError: (s) => (stderr += s) }),
        (e) => e instanceof CommandError && /no changes were expected but 3 changes occurred/.test(e.commandResult.stderr),
    );
    assert.match(stderr, /3 changes occurred/);
    await stack.workspace.removeStack("dev");
});

test("createStack rejects an existing stack; selectStack a missing one; tags are unsupported", async () => {
    const workDir = copyFixture("yaml-random");
    const opts = workspaceOptions();
    const created = await LocalWorkspace.createStack({ stackName: "dev", workDir }, opts);
    await assert.rejects(
        LocalWorkspace.createStack({ stackName: "dev", workDir }, opts),
        (e) => e instanceof StackAlreadyExistsError && e.cause?.kind === "stackExists",
    );
    await assert.rejects(
        LocalWorkspace.selectStack({ stackName: "missing", workDir }, opts),
        (e) => e instanceof StackNotFoundError && e.cause?.kind === "stackNotFound",
    );
    await assert.rejects(created.listTags(), CommandError);
    await created.workspace.removeStack("dev");
});

test("a failed resource is a CommandError whose cause is the library's ResourceOpFailedError", async () => {
    const workDir = copyFixture("yaml-fail");
    const stack = await LocalWorkspace.createOrSelectStack({ stackName: "dev", workDir }, workspaceOptions());
    const events = [];
    let output = "";
    await assert.rejects(
        stack.up({ onEvent: (e) => events.push(e), onOutput: (s) => (output += s) }),
        (e) => {
            assert.ok(e instanceof CommandError, `got ${e}`);
            assert.ok(e.cause instanceof ResourceOpFailedError);
            assert.match(e.cause.urn, /::boom$/);
            assert.match(e.commandResult.stderr, /meant to fail|exit status 3/);
            assert.equal(e.commandResult.code, 255);
            return true;
        },
    );
    assert.ok(events.some((e) => e.resOpFailedEvent?.metadata.urn.endsWith("::boom")));
    assert.ok(events.some((e) => e.diagnosticEvent?.severity === "error"));
    assert.match(output, /command:local:Command boom \*\*create failed\*\*/);
    assert.equal((await stack.info()).result, "failed");
    await stack.destroy();
    await stack.workspace.removeStack("dev");
});

test("an aborted signal cancels an up: cancelEvent, then 'update canceled'", async () => {
    const workDir = copyFixture("yaml-slow");
    const stack = await LocalWorkspace.createOrSelectStack({ stackName: "dev", workDir }, workspaceOptions());
    const controller = new AbortController();
    const events = [];
    await assert.rejects(
        stack.up({
            signal: controller.signal,
            onEvent: (e) => {
                events.push(e);
                if (e.resOutputsEvent?.metadata.urn.endsWith("::first")) {
                    controller.abort();
                }
            },
        }),
        (e) =>
            e instanceof CommandError &&
            e.cause instanceof CancelledError &&
            /error: update canceled/.test(e.commandResult.stderr),
    );
    assert.ok(events.some((e) => e.cancelEvent !== undefined));
    assert.ok(!events.some((e) => e.resOutputsEvent?.metadata.urn.endsWith("::slow3")));
    await stack.destroy();
    await stack.workspace.removeStack("dev");
});

test("inline program through the SDK's InlineProgramArgs shape", async () => {
    const pulumi = require("@pulumi/pulumi");
    const random = require("@pulumi/random");
    const program = async () => {
        const cfg = new pulumi.Config();
        const pet = new random.RandomPet("pet", { length: cfg.requireNumber("petLength") });
        return { petName: pet.id, secretKey: pulumi.secret("hush") };
    };
    const stack = await LocalWorkspace.createOrSelectStack(
        { stackName: "dev", projectName: "compat-inline", program },
        workspaceOptions({ stackSettings: { dev: { config: { petLength: "3" } } } }),
    );
    assert.equal(readProjectSettings(stack.workspace.workDir).name, "compat-inline");
    const up = await stack.up();
    assert.deepEqual(up.summary.resourceChanges, { create: 2 });
    assert.equal(up.outputs.secretKey.secret, true);
    assert.equal(up.outputs.secretKey.value, "hush");
    assert.equal(String(up.outputs.petName.value).split("-").length, 3);
    const destroy = await stack.destroy();
    assert.deepEqual(destroy.summary.resourceChanges, { delete: 2 });
    await stack.workspace.removeStack("dev");
});

test("createAutomationModule binds errors to the caller's SDK instance", async () => {
    const sdk = require("@pulumi/pulumi/automation");
    const mod = createAutomationModule({ sdk });
    assert.equal(mod.StackNotFoundError, sdk.StackNotFoundError);
    assert.equal(mod.PulumiCommand, PulumiCommand);
    assert.equal(typeof mod.parseAndValidatePulumiVersion, "function");
    const workDir = copyFixture("yaml-random");
    await assert.rejects(
        mod.LocalWorkspace.selectStack({ stackName: "nope", workDir }, workspaceOptions()),
        (e) => e instanceof sdk.StackNotFoundError && e instanceof sdk.CommandError && !(e instanceof StackNotFoundError),
    );
    const ws = await mod.LocalWorkspace.create({ workDir, ...workspaceOptions() });
    assert.equal(ws instanceof LocalWorkspace, true);
    // The SDK's own minimum-version check accepts the embedded version.
    const { minimumVersion } = require("@pulumi/pulumi/automation/minimumVersion");
    const cmd = await mod.PulumiCommand.get();
    assert.equal(mod.parseAndValidatePulumiVersion(minimumVersion, cmd.version.toString(), false).major, 3);
});
