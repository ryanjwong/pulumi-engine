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

/**
 * @pulumi-engine/node: the Pulumi engine as an in-process library.
 *
 * The engine runs inside this process (libpulumi, loaded over FFI); provider
 * plugins are child processes as the Pulumi protocol requires. No `pulumi`
 * CLI is involved.
 */

import { libraryPath, native } from "./native";
import { CancelledError, InvalidSpecError, PulumiError } from "./errors";
import { startInlineServer } from "./inline";

export * from "./errors";

/** Wrap a config value to mark it secret. */
export class Secret {
    constructor(readonly value: string) {}
}

export type ConfigInput = string | Secret | { value: string; secret?: boolean; object?: boolean };

export interface ConfigValue {
    value: string;
    secret?: boolean;
    object?: boolean;
}

export interface StackSpec {
    /** "dev" or "org/project/dev". */
    name: string;
    project: { name?: string; dir?: string; description?: string };
    /** file://, s3://, gs://, azblob:// or https:// backend. */
    backend: { url: string; token?: string; insecure?: boolean };
    /** "passphrase" (needs passphrase), "service", a KMS URL, or "b64" (tests only). */
    secrets?: { provider?: string; passphrase?: string };
    config?: Record<string, ConfigInput>;
    /**
     * Environment overlay for every provider plugin and language host the
     * operations on this stack launch. It never touches this process's own
     * environment; `Options.env` adds to it for one operation.
     */
    env?: Record<string, string>;
    /** Create the stack when it does not exist. */
    create?: boolean;
}

export interface Options {
    parallel?: number;
    message?: string;
    targets?: string[];
    targetDependents?: boolean;
    excludes?: string[];
    replaces?: string[];
    refresh?: boolean;
    continueOnError?: boolean;
    showSecrets?: boolean;
    /** Refresh/destroy as a preview. */
    dryRun?: boolean;
    /** Environment for the plugins and language host of this operation, over `StackSpec.env`. */
    env?: Record<string, string>;
    /** Preview only: write the proposed plan to this path (`pulumi preview --save-plan`). */
    savePlan?: string;
    /** Preview only: return the proposed plan in `Result.plan`. */
    generatePlan?: boolean;
    /** Up only: the path of a plan file the update is constrained to (`pulumi up --plan`). */
    plan?: string;
    /** Up only: the plan as a document (what `Result.plan` / a plan file holds) instead of a path. */
    planJson?: Plan;
    /** Aborting the signal cancels the operation gracefully. */
    signal?: AbortSignal;
}

/**
 * An update plan in the CLI's plan file format (`apitype.DeploymentPlanV1`):
 * the operations a preview proposed, with secret values encrypted by the
 * stack's secrets provider unless the preview ran with `showSecrets`.
 */
export interface Plan {
    manifest: { time: string; magic?: string; version?: string; plugins?: unknown };
    config?: Record<string, unknown>;
    resourcePlans?: Record<string, { goal?: unknown; seed?: string; steps?: string[]; outputs?: unknown }>;
    [other: string]: unknown;
}

/**
 * A program directory run by its language host. `languageVersion` pins the
 * runtime version the host should use ("1.38.5"), when the runtime supports it.
 */
export type LocalProgram = { mode: "local"; dir: string; languageVersion?: string };
export type CallbackProgram = { mode: "callback"; address: string };
/** An inline program: runs in this process through @pulumi/pulumi's LanguageServer. */
export type InlineProgram = () => void | Promise<void> | Promise<Record<string, unknown>>;
export type Program = LocalProgram | CallbackProgram | InlineProgram;

export type EventType =
    | "stdout"
    | "diagnostic"
    | "prelude"
    | "summary"
    | "resourcePre"
    | "resourceOutputs"
    | "resourceOpFailed"
    | "policy"
    | "policyRemediation"
    | "policyLoad"
    | "policyAnalyzeSummary"
    | "policyRemediateSummary"
    | "policyAnalyzeStackSummary"
    | "startDebugging"
    | "progress"
    | "error"
    | "cancel"
    | "unknown";

/**
 * One engine event: Pulumi's engine event JSON plus a `type` discriminator.
 * `sequence` counts from 0 within the operation and `timestamp` is epoch
 * seconds; every stream ends with a `cancel` event after the summary.
 */
export interface Event {
    type: EventType;
    sequence: number;
    timestamp: number;
    cancelEvent?: Record<string, never>;
    diagnosticEvent?: { urn?: string; prefix?: string; message: string; severity: string; ephemeral?: boolean };
    preludeEvent?: { config: Record<string, string> };
    summaryEvent?: { maybeCorrupt: boolean; durationSeconds: number; resourceChanges: Record<string, number>; isPreview: boolean };
    resourcePreEvent?: { metadata: StepEventMetadata; planning?: boolean };
    resOutputsEvent?: { metadata: StepEventMetadata; planning?: boolean };
    resOpFailedEvent?: { metadata: StepEventMetadata; status: number; steps: number };
    stdoutEvent?: { message: string };
    [other: string]: unknown;
}

export interface StepEventMetadata {
    op: string;
    urn: string;
    type: string;
    old?: StepEventStateMetadata;
    new?: StepEventStateMetadata;
    keys?: string[];
    diffs?: string[];
    provider?: string;
    [other: string]: unknown;
}

export interface StepEventStateMetadata {
    type: string;
    urn: string;
    id?: string;
    parent?: string;
    inputs?: Record<string, unknown>;
    outputs?: Record<string, unknown>;
    [other: string]: unknown;
}

export interface Outputs {
    values: Record<string, unknown>;
    secretKeys: string[];
}

export interface Result {
    kind: "preview" | "up" | "refresh" | "destroy";
    summary?: Event["summaryEvent"];
    changes: Record<string, number>;
    outputs?: Outputs;
    failures?: { URN: string; Type: string; Op: string; Provider: string; Message: string }[];
    cancelled?: boolean;
    /** The plan a preview generated (`options.savePlan` or `options.generatePlan`). */
    plan?: Plan;
    durationMs: number;
}

/** One entry of a stack's update history, newest first. */
export interface UpdateInfo {
    /** "update", "preview", "refresh", "destroy", "import", "rename". */
    kind: string;
    /** "succeeded", "failed", "in-progress" or "not-started". */
    result: string;
    message: string;
    /** Epoch seconds. */
    startTime: number;
    /** Epoch seconds. */
    endTime: number;
    /** The update's sequence number on HTTP backends; DIY backends report 0. */
    version: number;
    environment: Record<string, string>;
    config: Record<string, ConfigValue>;
    resourceChanges?: Record<string, number>;
}

/** Options for {@link Stack.history}. */
export interface HistoryOptions {
    /** Maximum entries (0 or unset: what the backend returns). */
    limit?: number;
    /** Page of `limit` entries, from 1. */
    page?: number;
    /** Decrypt secret config values with the stack's secrets manager. */
    showSecrets?: boolean;
}

/** One entry of a stack listing. */
export interface StackSummary {
    /** The reference as the backend renders it for a listing ("dev"). */
    name: string;
    /** The fully qualified reference ("org/project/dev"). */
    fullName: string;
    project?: string;
    /** ISO-8601 time of the last update, when the backend knows it. */
    lastUpdate?: string;
    resourceCount?: number;
}

/** Filter for {@link listStacks}. */
export interface ListFilter {
    project?: string;
    /** HTTP backends only; DIY backends reject it. */
    organization?: string;
    tagName?: string;
    tagValue?: string;
}

/** List the stacks a backend holds. */
export function listStacks(backend: StackSpec["backend"], filter: ListFilter = {}): StackSummary[] {
    return JSON.parse(native().listStacks(JSON.stringify({ backend, filter }))) as StackSummary[];
}

/** Version of the library and of the Pulumi packages it embeds. */
export function version(): string {
    return native().version();
}

function normaliseConfig(config?: Record<string, ConfigInput>): Record<string, ConfigValue> | undefined {
    if (!config) {
        return undefined;
    }
    const out: Record<string, ConfigValue> = {};
    for (const [k, v] of Object.entries(config)) {
        if (v instanceof Secret) {
            out[k] = { value: v.value, secret: true };
        } else if (typeof v === "string") {
            out[k] = { value: v };
        } else {
            out[k] = v;
        }
    }
    return out;
}

/** Open (or with `create: true`, create) a stack. */
export async function openStack(spec: StackSpec): Promise<Stack> {
    const wire = { ...spec, config: normaliseConfig(spec.config) };
    const handle = native().stackOpen(JSON.stringify(wire));
    return new Stack(handle, spec.name);
}

/**
 * A running or finished operation. Iterate it for events (`for await`), and
 * call `result()` for the outcome. Both may be used at once; neither is
 * required. `result()` rejects with a typed PulumiError subclass.
 */
export class Operation implements AsyncIterable<Event> {
    private readonly resultPromise: Promise<Result>;
    private released = false;
    private readonly cleanups: (() => Promise<void> | void)[] = [];

    /** @internal */
    constructor(
        readonly kind: Result["kind"],
        private readonly id: number,
        signal?: AbortSignal,
        cleanup?: () => Promise<void> | void,
    ) {
        if (cleanup) {
            this.cleanups.push(cleanup);
        }
        const onAbort = () => this.cancel();
        if (signal) {
            if (signal.aborted) {
                onAbort();
            } else {
                signal.addEventListener("abort", onAbort, { once: true });
                this.cleanups.push(() => signal.removeEventListener("abort", onAbort));
            }
        }
        this.resultPromise = (async () => {
            try {
                const json = await native().opWait(id);
                return JSON.parse(json) as Result;
            } finally {
                for (const c of this.cleanups) {
                    await c();
                }
            }
        })();
        // Avoid unhandled rejection warnings when the caller only iterates.
        this.resultPromise.catch(() => undefined);
    }

    /** Graceful cancel; a second call terminates the engine. */
    cancel(): void {
        if (!this.released) {
            native().opCancel(this.id);
        }
    }

    /** Resolves with the result, or rejects with a typed error. */
    result(): Promise<Result> {
        return this.resultPromise;
    }

    async *[Symbol.asyncIterator](): AsyncIterator<Event> {
        for (;;) {
            const json = await native().opNextEvent(this.id, -1);
            if (json === null) {
                return;
            }
            if (json === "") {
                continue;
            }
            yield JSON.parse(json) as Event;
        }
    }

    /** Collect every event into an array. */
    async events(): Promise<Event[]> {
        const out: Event[] = [];
        for await (const e of this) {
            out.push(e);
        }
        return out;
    }

    /** Release the native handle once the operation is done with. */
    async release(): Promise<void> {
        if (this.released) {
            return;
        }
        await this.resultPromise.catch(() => undefined);
        this.released = true;
        native().opRelease(this.id);
    }
}

export class Stack {
    private closed = false;

    /** @internal */
    constructor(
        private readonly handle: number,
        readonly name: string,
    ) {}

    private ensureOpen(): void {
        if (this.closed) {
            throw new InvalidSpecError("stack", "stack handle is closed");
        }
    }

    private async start(kind: Result["kind"], program: Program | undefined, options: Options = {}): Promise<Operation> {
        this.ensureOpen();
        const { signal, ...opts } = options;
        let wireProgram: LocalProgram | CallbackProgram | undefined;
        let cleanup: (() => Promise<void>) | undefined;
        if (typeof program === "function") {
            const server = await startInlineServer(program);
            wireProgram = { mode: "callback", address: server.address };
            cleanup = server.close;
        } else {
            wireProgram = program;
        }
        if ((kind === "up" || kind === "preview") && !wireProgram) {
            throw new InvalidSpecError("program", `${kind} requires a program`);
        }
        const request = { kind, program: wireProgram, options: opts };
        let id: number;
        try {
            id = native().opStart(this.handle, JSON.stringify(request));
        } catch (e) {
            await cleanup?.();
            throw e;
        }
        return new Operation(kind, id, signal, cleanup);
    }

    preview(program: Program, options?: Options): Promise<Operation> {
        return this.start("preview", program, options);
    }
    up(program: Program, options?: Options): Promise<Operation> {
        return this.start("up", program, options);
    }
    refresh(program?: Program, options?: Options): Promise<Operation> {
        return this.start("refresh", program, options);
    }
    destroy(program?: Program, options?: Options): Promise<Operation> {
        return this.start("destroy", program, options);
    }

    /** Stack outputs; secrets redacted unless showSecrets. */
    outputs(showSecrets = false): Outputs {
        this.ensureOpen();
        return JSON.parse(native().stackOutputs(this.handle, showSecrets)) as Outputs;
    }

    /** The checkpoint as Pulumi's untyped deployment ({version, deployment}). */
    export(): unknown {
        this.ensureOpen();
        return JSON.parse(native().stackExport(this.handle));
    }

    import(deployment: unknown): void {
        this.ensureOpen();
        native().stackImport(this.handle, JSON.stringify(deployment));
    }

    setConfig(key: string, value: ConfigInput): void {
        this.ensureOpen();
        const v = normaliseConfig({ [key]: value })![key];
        native().stackSetConfig(this.handle, key, JSON.stringify(v));
    }

    getConfig(key: string): ConfigValue | undefined {
        this.ensureOpen();
        const v = JSON.parse(native().stackGetConfig(this.handle, key));
        return v === null ? undefined : (v as ConfigValue);
    }

    /** The stack's tags. */
    getTags(): Record<string, string> {
        this.ensureOpen();
        return JSON.parse(native().stackGetTags(this.handle)) as Record<string, string>;
    }

    /** Replace every tag on the stack. */
    setTags(tags: Record<string, string>): void {
        this.ensureOpen();
        native().stackSetTags(this.handle, JSON.stringify(tags));
    }

    /** The stack's update history, newest first. Previews are not recorded. */
    history(options: HistoryOptions = {}): UpdateInfo[] {
        this.ensureOpen();
        return JSON.parse(native().stackHistory(this.handle, JSON.stringify(options))) as UpdateInfo[];
    }

    /** Cancel running operations (and the backend's current update where supported). */
    cancel(): void {
        this.ensureOpen();
        native().stackCancel(this.handle);
    }

    /** Delete the stack from the backend. */
    remove(force = false): void {
        this.ensureOpen();
        native().stackRemove(this.handle, force);
    }

    /** Release the native handle. */
    close(): void {
        if (!this.closed) {
            this.closed = true;
            native().stackClose(this.handle);
        }
    }
}

export { CancelledError, PulumiError };
/** The shared library the binding loads (or would load): `$PULUMI_ENGINE_LIB`, the platform package, or an in-tree build. */
export { libraryPath };
