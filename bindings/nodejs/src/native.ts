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

// The koffi layer: loads libpulumi and exposes its C ABI as plain functions.
// Strings returned by the library are decoded and released with pulumi_free
// here, so nothing above this file sees a raw pointer. The two blocking
// calls (next_event, wait) go through koffi's async path so that the Node
// event loop never blocks.

import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import koffi from "koffi";

import { PulumiError, errorFromJSON } from "./errors";

/** The platform key the packages are named by: "darwin-arm64", "linux-amd64". */
export function platformKey(): string {
    const arch = { x64: "amd64", arm64: "arm64" }[os.arch()] ?? os.arch();
    return `${os.platform()}-${arch}`;
}

function libraryExt(): string {
    return os.platform() === "darwin" ? "dylib" : "so";
}

/**
 * The per-platform packages this build declares (written by
 * scripts/package-node.mjs into the published package.json as
 * `pulumiEngine.platformPackages`), else the conventional names for both the
 * publish scope and the source scope.
 */
function platformPackageNames(key: string): string[] {
    const names: string[] = [];
    try {
        // eslint-disable-next-line @typescript-eslint/no-require-imports
        const pkg = require(path.join(__dirname, "..", "..", "package.json")) as {
            pulumiEngine?: { platformPackages?: Record<string, string> };
        };
        const declared = pkg.pulumiEngine?.platformPackages?.[key];
        if (declared) {
            names.push(declared);
        }
    } catch {
        // no package.json next to the build (bundled); fall through
    }
    for (const n of [`@ryanjwong/pulumi-engine-node-${key}`, `@pulumi-engine/node-${key}`]) {
        if (!names.includes(n)) {
            names.push(n);
        }
    }
    return names;
}

/**
 * Locate the shared library, in order: `$PULUMI_ENGINE_LIB`; the platform
 * package for this OS/CPU (an optionalDependency of the published package,
 * resolved from this package's location, so a hoisted or nested install both
 * work); the in-tree `lib/native/` copy `make node` makes; the repository's
 * `build/` directory.
 */
export function libraryPath(): string {
    const override = process.env.PULUMI_ENGINE_LIB;
    if (override) {
        return override;
    }
    const key = platformKey();
    const ext = libraryExt();
    const tried: string[] = [];
    for (const name of platformPackageNames(key)) {
        try {
            const dir = path.dirname(require.resolve(`${name}/package.json`, { paths: [__dirname] }));
            const candidate = path.join(dir, `libpulumi.${ext}`);
            if (fs.existsSync(candidate)) {
                return candidate;
            }
            tried.push(candidate);
        } catch {
            tried.push(`${name} (not installed)`);
        }
    }
    const inTree = [
        path.join(__dirname, "..", "..", "lib", "native", `libpulumi-${key}.${ext}`),
        path.join(__dirname, "..", "..", "..", "..", "build", `libpulumi.${ext}`),
    ];
    for (const c of inTree) {
        if (fs.existsSync(c)) {
            return c;
        }
        tried.push(c);
    }
    throw new Error(
        `libpulumi not found for ${key}. Install the platform package ${platformPackageNames(key)[0]} ` +
            `(an optionalDependency of this package; check that optional dependencies are not disabled and that ` +
            `${key} is a released platform), build it with 'make lib', or set PULUMI_ENGINE_LIB. Looked in: ${tried.join(", ")}.`,
    );
}

type Ptr = unknown;

export interface Native {
    version(): string;
    stackOpen(specJson: string): number;
    stackClose(handle: number): void;
    opStart(handle: number, requestJson: string): number;
    /** Resolves to the event JSON, "" on timeout, or null at end of stream. */
    opNextEvent(id: number, timeoutMs: number): Promise<string | null>;
    opCancel(id: number): void;
    /** Resolves to the result JSON or rejects with a typed PulumiError. */
    opWait(id: number): Promise<string>;
    opRelease(id: number): void;
    stackExport(handle: number): string;
    stackImport(handle: number, deploymentJson: string): void;
    stackOutputs(handle: number, showSecrets: boolean): string;
    stackSetConfig(handle: number, key: string, valueJson: string): void;
    stackGetConfig(handle: number, key: string): string;
    stackRemove(handle: number, force: boolean): void;
    stackCancel(handle: number): void;
    stackGetTags(handle: number): string;
    stackSetTags(handle: number, tagsJson: string): void;
    stackHistory(handle: number, optionsJson: string): string;
    listStacks(requestJson: string): string;
}

let cached: Native | undefined;

/** Load (once) and return the native bindings. */
export function native(): Native {
    if (cached) {
        return cached;
    }
    const lib = koffi.load(libraryPath());
    const free = lib.func("void pulumi_free(void *p)");
    // Strings the library hands back: decode, then release.
    const take = (p: Ptr): string | null => {
        if (p === null || p === undefined) {
            return null;
        }
        try {
            return koffi.decode(p, "char", -1) as string;
        } finally {
            free(p);
        }
    };
    const outErr = koffi.out(koffi.pointer("void *"));
    // Turn an error out-parameter into a thrown PulumiError.
    const check = (err: Ptr[]): void => {
        const s = take(err[0]);
        if (s !== null) {
            throw errorFromJSON(s);
        }
    };

    const f = {
        version: lib.func("void *pulumi_version()"),
        stackOpen: lib.func("pulumi_stack_open", "int64_t", ["str", outErr]),
        stackClose: lib.func("int pulumi_stack_close(int64_t h)"),
        opStart: lib.func("pulumi_op_start", "int64_t", ["int64_t", "str", outErr]),
        opNextEvent: lib.func("void *pulumi_op_next_event(int64_t id, int32_t timeoutMs)"),
        opCancel: lib.func("int pulumi_op_cancel(int64_t id)"),
        opWait: lib.func("pulumi_op_wait", "void *", ["int64_t", outErr]),
        opRelease: lib.func("int pulumi_op_release(int64_t id)"),
        stackExport: lib.func("pulumi_stack_export", "void *", ["int64_t", outErr]),
        stackImport: lib.func("pulumi_stack_import", "int", ["int64_t", "str", outErr]),
        stackOutputs: lib.func("pulumi_stack_outputs", "void *", ["int64_t", "int", outErr]),
        stackSetConfig: lib.func("pulumi_stack_set_config", "int", ["int64_t", "str", "str", outErr]),
        stackGetConfig: lib.func("pulumi_stack_get_config", "void *", ["int64_t", "str", outErr]),
        stackRemove: lib.func("pulumi_stack_remove", "int", ["int64_t", "int", outErr]),
        stackCancel: lib.func("pulumi_stack_cancel", "int", ["int64_t", outErr]),
        stackGetTags: lib.func("pulumi_stack_get_tags", "void *", ["int64_t", outErr]),
        stackSetTags: lib.func("pulumi_stack_set_tags", "int", ["int64_t", "str", outErr]),
        stackHistory: lib.func("pulumi_stack_history", "void *", ["int64_t", "str", outErr]),
        listStacks: lib.func("pulumi_list_stacks", "void *", ["str", outErr]),
    };

    const unknown = (what: string, id: number) =>
        new PulumiError("invalidSpec", `unknown ${what} ${id} (already released?)`);

    cached = {
        version: () => take(f.version()) ?? "",
        stackOpen: (spec) => {
            const err: Ptr[] = [null];
            const h = Number(f.stackOpen(spec, err));
            check(err);
            return h;
        },
        stackClose: (h) => {
            if (f.stackClose(h) !== 0) {
                throw unknown("stack handle", h);
            }
        },
        opStart: (h, req) => {
            const err: Ptr[] = [null];
            const id = Number(f.opStart(h, req, err));
            check(err);
            return id;
        },
        opNextEvent: (id, timeoutMs) =>
            new Promise<string | null>((resolve, reject) => {
                f.opNextEvent.async(id, timeoutMs, (e: unknown, p: Ptr) => {
                    if (e) {
                        reject(e);
                        return;
                    }
                    resolve(take(p));
                });
            }),
        opCancel: (id) => {
            if (f.opCancel(id) !== 0) {
                throw unknown("operation", id);
            }
        },
        opWait: (id) =>
            new Promise<string>((resolve, reject) => {
                const err: Ptr[] = [null];
                f.opWait.async(id, err, (e: unknown, p: Ptr) => {
                    if (e) {
                        reject(e);
                        return;
                    }
                    const res = take(p);
                    try {
                        check(err);
                    } catch (typed) {
                        reject(typed);
                        return;
                    }
                    resolve(res ?? "{}");
                });
            }),
        opRelease: (id) => {
            f.opRelease(id);
        },
        stackExport: (h) => {
            const err: Ptr[] = [null];
            const s = take(f.stackExport(h, err));
            check(err);
            return s ?? "";
        },
        stackImport: (h, dep) => {
            const err: Ptr[] = [null];
            f.stackImport(h, dep, err);
            check(err);
        },
        stackOutputs: (h, show) => {
            const err: Ptr[] = [null];
            const s = take(f.stackOutputs(h, show ? 1 : 0, err));
            check(err);
            return s ?? "";
        },
        stackSetConfig: (h, key, value) => {
            const err: Ptr[] = [null];
            f.stackSetConfig(h, key, value, err);
            check(err);
        },
        stackGetConfig: (h, key) => {
            const err: Ptr[] = [null];
            const s = take(f.stackGetConfig(h, key, err));
            check(err);
            return s ?? "null";
        },
        stackRemove: (h, force) => {
            const err: Ptr[] = [null];
            f.stackRemove(h, force ? 1 : 0, err);
            check(err);
        },
        stackCancel: (h) => {
            const err: Ptr[] = [null];
            f.stackCancel(h, err);
            check(err);
        },
        stackGetTags: (h) => {
            const err: Ptr[] = [null];
            const s = take(f.stackGetTags(h, err));
            check(err);
            return s ?? "{}";
        },
        stackSetTags: (h, tags) => {
            const err: Ptr[] = [null];
            f.stackSetTags(h, tags, err);
            check(err);
        },
        stackHistory: (h, options) => {
            const err: Ptr[] = [null];
            const s = take(f.stackHistory(h, options, err));
            check(err);
            return s ?? "[]";
        },
        listStacks: (request) => {
            const err: Ptr[] = [null];
            const s = take(f.listStacks(request, err));
            check(err);
            return s ?? "[]";
        },
    };
    return cached;
}
