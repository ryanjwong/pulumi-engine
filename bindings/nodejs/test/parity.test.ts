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

// CLI parity for the Node binding: one inline Node program, one file://
// backend, one passphrase; the binding and the real `pulumi` CLI must agree
// on state, secrets and outputs in both directions. The same JavaScript file
// is the inline program for the binding and the entry point the CLI's Node
// language host runs. Skipped when no `pulumi` binary is available
// (PULUMI_ENGINE_CLI overrides the one on PATH). See docs/cli-parity.md.

import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { test } from "node:test";

import { Secret, openStack, type InlineProgram } from "../src";

const passphrase = "correct horse battery staple";
const cliBin = process.env.PULUMI_ENGINE_CLI ?? "pulumi";

function cliVersion(): string | undefined {
    try {
        return execFileSync(cliBin, ["version"], { encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] }).trim();
    } catch {
        return undefined;
    }
}
const version = cliVersion();
const skip = version ? false : `no pulumi CLI (${cliBin}); set PULUMI_ENGINE_CLI`;

function tmp(prefix: string): string {
    return fs.mkdtempSync(path.join(os.tmpdir(), `pulumi-engine-parity-${prefix}-`));
}

const secretSig = "4dabf18193072939515e22adb298388d";
function isCiphertext(v: unknown): boolean {
    const m = v as Record<string, unknown> | null;
    return !!m && typeof m === "object" && m[secretSig] === "1b47061264138c4ac30d75fd1eb44270" && typeof m.ciphertext === "string";
}

interface Cli {
    state: string;
    run(dir: string, args: string[]): string;
    json<T>(dir: string, args: string[]): T;
    fails(dir: string, args: string[]): boolean;
}

function cli(): Cli {
    const state = tmp("state");
    const env = {
        ...process.env,
        PULUMI_BACKEND_URL: "file://" + state,
        PULUMI_CONFIG_PASSPHRASE: passphrase,
        PULUMI_SKIP_UPDATE_CHECK: "true",
        PULUMI_SKIP_CONFIRMATIONS: "true",
        NO_COLOR: "1",
    };
    const exec = (dir: string, args: string[]) =>
        spawnSync(cliBin, [...args, "--non-interactive"], { cwd: dir, env, encoding: "utf8", maxBuffer: 64 << 20 });
    return {
        state,
        run(dir, args) {
            const r = exec(dir, args);
            if (r.status !== 0) {
                throw new Error(`pulumi ${args.join(" ")} exited ${r.status}\n${r.stdout}\n${r.stderr}`);
            }
            return r.stdout;
        },
        json<T>(dir: string, args: string[]): T {
            return JSON.parse(this.run(dir, args)) as T;
        },
        fails(dir, args) {
            return exec(dir, args).status !== 0;
        },
    };
}

// project writes a Node Pulumi project whose entry point exports the
// program function, so the CLI runs the very function the binding runs inline.
function project(): { dir: string; program: InlineProgram } {
    const dir = tmp("proj");
    fs.writeFileSync(path.join(dir, "Pulumi.yaml"), "name: node-parity\nruntime: nodejs\nmain: index.js\n");
    fs.writeFileSync(path.join(dir, "package.json"), JSON.stringify({ name: "node-parity", main: "index.js" }));
    // The binding's node_modules (realpath-resolved, so the inline program
    // and the binding share one @pulumi/pulumi instance).
    fs.symlinkSync(path.resolve(__dirname, "..", "..", "node_modules"), path.join(dir, "node_modules"), "dir");
    fs.writeFileSync(
        path.join(dir, "index.js"),
        `const pulumi = require("@pulumi/pulumi");
const random = require("@pulumi/random");
module.exports = async () => {
    const cfg = new pulumi.Config();
    const pet = new random.RandomPet("pet", { length: cfg.requireNumber("petLength") });
    const pw = new random.RandomPassword("pw", { length: 12 });
    return { petName: pet.id, password: pw.result, apiKey: cfg.requireSecret("apiKey") };
};
`,
    );
    // eslint-disable-next-line @typescript-eslint/no-require-imports
    const program = require(path.join(dir, "index.js")) as InlineProgram;
    return { dir, program };
}

test(`parity: binding writes, CLI (${version ?? "none"}) reads`, { skip }, async () => {
    const c = cli();
    const { dir, program } = project();
    const stack = await openStack({
        name: "dev",
        project: { name: "node-parity", dir },
        backend: { url: "file://" + c.state },
        secrets: { provider: "passphrase", passphrase },
        config: { petLength: "3", apiKey: new Secret("hunter2") },
        create: true,
    });

    const up = await stack.up(program);
    await up.events();
    const upResult = await up.result();
    await up.release();
    assert.equal(upResult.changes.create, 3);
    const shown = stack.outputs(true);
    assert.equal(String(shown.values.password).length, 12);
    assert.equal(shown.values.apiKey, "hunter2");

    // The checkpoint the CLI exports is the one the binding exports, with
    // secret outputs as ciphertext.
    const cliExport = c.json<{ version: number; deployment: { resources: { type: string; outputs?: Record<string, unknown> }[] } }>(
        dir,
        ["stack", "export", "--stack", "dev"],
    );
    assert.equal(cliExport.version, 3);
    assert.deepEqual(cliExport, stack.export());
    const root = cliExport.deployment.resources.find((r) => r.type === "pulumi:pulumi:Stack");
    assert.ok(root?.outputs, "stack resource with outputs");
    assert.ok(isCiphertext(root.outputs.password), `password in checkpoint: ${JSON.stringify(root.outputs.password)}`);
    assert.ok(isCiphertext(root.outputs.apiKey), `apiKey in checkpoint: ${JSON.stringify(root.outputs.apiKey)}`);
    assert.equal(root.outputs.petName, shown.values.petName);
    assert.ok(!JSON.stringify(cliExport).includes(String(shown.values.password)), "plaintext secret in checkpoint");

    // stack output: redacted, then plaintext under --show-secrets
    const redacted = c.json<Record<string, unknown>>(dir, ["stack", "output", "--json", "--stack", "dev"]);
    assert.deepEqual(redacted, stack.outputs(false).values);
    assert.equal(redacted.password, "[secret]");
    assert.equal(redacted.apiKey, "[secret]");
    const cliShown = c.json<Record<string, unknown>>(dir, ["stack", "output", "--json", "--show-secrets", "--stack", "dev"]);
    assert.deepEqual(cliShown, shown.values);

    // config as the CLI sees it
    assert.equal(c.run(dir, ["config", "get", "petLength", "--stack", "dev"]).trim(), "3");
    assert.equal(c.run(dir, ["config", "get", "apiKey", "--stack", "dev"]).trim(), "hunter2");

    // The CLI runs the same program through its Node language host and
    // finds nothing to do; then refreshes and destroys.
    c.run(dir, ["preview", "--expect-no-changes", "--stack", "dev"]);
    c.run(dir, ["refresh", "--yes", "--skip-preview", "--expect-no-changes", "--stack", "dev"]);
    c.run(dir, ["destroy", "--yes", "--skip-preview", "--stack", "dev"]);
    assert.deepEqual(stack.outputs(false).values, {});
    c.run(dir, ["stack", "rm", "--yes", "--stack", "dev"]);
    stack.close();
    await assert.rejects(
        openStack({
            name: "dev",
            project: { name: "node-parity", dir },
            backend: { url: "file://" + c.state },
            secrets: { provider: "passphrase", passphrase },
        }),
        (e: unknown) => (e as { kind?: string }).kind === "stackNotFound",
    );
});

test(`parity: CLI (${version ?? "none"}) writes, binding reads`, { skip }, async () => {
    const c = cli();
    const { dir, program } = project();
    c.run(dir, ["stack", "init", "dev"]);
    c.run(dir, ["config", "set", "petLength", "3", "--stack", "dev"]);
    c.run(dir, ["config", "set", "--secret", "apiKey", "hunter2", "--stack", "dev"]);
    c.run(dir, ["up", "--yes", "--skip-preview", "--stack", "dev"]);
    const cliShown = c.json<Record<string, unknown>>(dir, ["stack", "output", "--json", "--show-secrets", "--stack", "dev"]);

    const stack = await openStack({
        name: "dev",
        project: { name: "node-parity", dir },
        backend: { url: "file://" + c.state },
        secrets: { provider: "passphrase", passphrase },
    });
    assert.deepEqual(stack.getConfig("apiKey"), { value: "hunter2", secret: true });
    assert.deepEqual(stack.getConfig("petLength"), { value: "3" });

    const preview = await stack.preview(program);
    await preview.events();
    const previewResult = await preview.result();
    await preview.release();
    assert.equal(previewResult.changes.same, 3, JSON.stringify(previewResult.changes));
    assert.equal(previewResult.changes.create ?? 0, 0);
    assert.equal(previewResult.changes.update ?? 0, 0);

    const shown = stack.outputs(true);
    assert.deepEqual(shown.values, cliShown);
    assert.equal(String(shown.values.password).length, 12);
    assert.equal(shown.values.apiKey, "hunter2");
    assert.deepEqual(shown.secretKeys, ["apiKey", "password"]);
    assert.equal(stack.outputs(false).values.password, "[secret]");

    const refresh = await stack.refresh();
    await refresh.events();
    const refreshResult = await refresh.result();
    await refresh.release();
    assert.ok((refreshResult.changes.same ?? 0) > 0, JSON.stringify(refreshResult.changes));
    assert.equal(refreshResult.changes.update ?? 0, 0);
    assert.equal(refreshResult.changes.delete ?? 0, 0);

    const destroy = await stack.destroy();
    await destroy.events();
    const destroyResult = await destroy.result();
    await destroy.release();
    assert.equal(destroyResult.changes.delete, 3);

    type StackRow = { name: string; resourceCount?: number };
    const listed = c.json<StackRow[]>(dir, ["stack", "ls", "--json"]).find((s) => s.name === "dev");
    assert.ok(listed, "CLI lists the stack");
    assert.equal(listed.resourceCount ?? 0, 0);
    stack.remove();
    stack.close();
    assert.ok(!c.json<StackRow[]>(dir, ["stack", "ls", "--json"]).some((s) => s.name === "dev"));
    assert.ok(c.fails(dir, ["stack", "rm", "--yes", "--stack", "dev"]));
});
