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
 * `@pulumi-engine/node/automation-compat`: the subset of
 * `@pulumi/pulumi/automation` that Automation API consumers drive, served by
 * the in-process engine instead of a `pulumi` subprocess.
 *
 * The shapes are the SDK's own — `LocalWorkspace.createOrSelectStack`,
 * `Stack.up/preview/refresh/destroy/previewDestroy` with the SDK's option
 * bags (`onEvent`, `onOutput`, `onError`, `signal`, `diff`, `expectNoChanges`,
 * `parallel`, `message`, `target`…), `UpResult`/`PreviewResult`-shaped
 * results, `EngineEvent` objects with the SDK's field names — so code written
 * against the SDK runs unchanged when the module is swapped in. Every place
 * the facade diverges from the SDK is listed in the README ("Automation API
 * compatibility").
 *
 * Errors: when the consumer hands in its own `@pulumi/pulumi/automation`
 * instance ({@link createAutomationModule}), failures are built with that
 * SDK's `createCommandError`/`CommandResult`, so the consumer's `instanceof
 * StackNotFoundError` / `ConcurrentUpdateError` / `CommandError` checks hold.
 * Without an SDK, structurally identical classes exported from this module
 * are used. Either way the library's typed `PulumiError` rides on `cause`.
 *
 * The library supplies what the facade used to synthesise: engine events
 * carry their own `sequence`/`timestamp` and the stream's closing
 * `cancelEvent`, `history`/`info` read the backend's update history, tags go
 * through the backend, `listStacks` works on every backend, and the
 * workspace's `envVars` reach plugins and language hosts as `StackSpec.env`.
 */

import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

import {
    CancelledError,
    ConcurrentUpdateError as EngineConcurrentUpdateError,
    PulumiError,
    Secret,
    StackExistsError,
    StackNotFoundError as EngineStackNotFoundError,
    UnsupportedError,
    listStacks as libraryListStacks,
    openStack,
    version as libraryVersion,
    type ConfigValue as EngineConfigValue,
    type Event as EngineEvent_,
    type InlineProgram,
    type Options as EngineOptions,
    type Program,
    type Result as EngineResult,
    type Stack as EngineStack,
    type StackSpec,
    type UpdateInfo as EngineUpdateInfo,
} from "./index";
import { libraryPath } from "./native";

// ── SDK-shaped types ─────────────────────────────────────────────────────────

/** The SDK's `EngineEvent`: Pulumi's engine event JSON, one `*Event` field set. */
export interface EngineEvent {
    sequence: number;
    timestamp: number;
    cancelEvent?: Record<string, never>;
    stdoutEvent?: { message: string; color: string };
    diagnosticEvent?: {
        urn?: string;
        prefix?: string;
        message: string;
        color: string;
        severity: "info" | "info#err" | "warning" | "error" | "debug" | string;
        streamID?: number;
        ephemeral?: boolean;
    };
    preludeEvent?: { config: Record<string, string> };
    summaryEvent?: SummaryEvent;
    resourcePreEvent?: { metadata: StepEventMetadata; planning?: boolean };
    resOutputsEvent?: { metadata: StepEventMetadata; planning?: boolean };
    resOpFailedEvent?: { metadata: StepEventMetadata; status: number; steps: number };
    policyEvent?: unknown;
    [other: string]: unknown;
}

export interface SummaryEvent {
    maybeCorrupt: boolean;
    durationSeconds: number;
    resourceChanges: OpMap;
    isPreview?: boolean;
    result?: string;
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
    detailedDiff?: Record<string, unknown>;
    logical?: boolean;
    provider?: string;
    [other: string]: unknown;
}

export interface StepEventStateMetadata {
    type: string;
    urn: string;
    custom?: boolean;
    delete?: boolean;
    id?: string;
    parent?: string;
    protect?: boolean;
    inputs?: Record<string, unknown>;
    outputs?: Record<string, unknown>;
    provider?: string;
    [other: string]: unknown;
}

export type OpMap = { [op: string]: number | undefined };

export interface OutputValue {
    value: unknown;
    secret: boolean;
}
export type OutputMap = { [key: string]: OutputValue };

export interface ConfigValue {
    value: string;
    secret?: boolean;
}
export type ConfigMap = { [key: string]: ConfigValue };

export type UpdateKind = "update" | "preview" | "refresh" | "rename" | "destroy" | "import";
export type UpdateResult = "not-started" | "in-progress" | "succeeded" | "failed";

export interface UpdateSummary {
    kind: UpdateKind;
    startTime: Date;
    message: string;
    environment: { [key: string]: string };
    config: ConfigMap;
    result: UpdateResult;
    endTime: Date;
    version: number;
    Deployment?: unknown;
    resourceChanges?: OpMap;
}

export interface Deployment {
    version: number;
    deployment: unknown;
}

export interface StackSummary {
    name: string;
    current: boolean;
    lastUpdate?: string;
    updateInProgress?: boolean;
    resourceCount?: number;
    url?: string;
}

/** The option fields shared by every stack operation. */
export interface GlobalOpts {
    onOutput?: (out: string) => void;
    onError?: (err: string) => void;
    onEvent?: (event: EngineEvent) => void;
    color?: "always" | "never" | "raw" | "auto";
    logFlow?: boolean;
    logVerbosity?: number;
    logToStdErr?: boolean;
    tracing?: string;
    debug?: boolean;
    signal?: AbortSignal;
    showSecrets?: boolean;
    parallel?: number;
    message?: string;
    /** Set by the facade for `previewDestroy`; not part of the SDK's bags. */
    [other: string]: unknown;
}

export interface UpOptions extends GlobalOpts {
    expectNoChanges?: boolean;
    diff?: boolean;
    replace?: string[];
    target?: string[];
    targetDependents?: boolean;
    exclude?: string[];
    excludeDependents?: boolean;
    /** Path of a plan file the update is constrained to (`pulumi up --plan`); a mismatch is a `CommandError` whose `cause` is `PlanViolationError`. */
    plan?: string;
    program?: PulumiFn;
    refresh?: boolean;
    continueOnError?: boolean;
    userAgent?: string;
    policyPacks?: string[];
    policyPackConfigs?: string[];
    importFile?: string;
    attachDebugger?: boolean;
    suppressOutputs?: boolean;
    suppressProgress?: boolean;
    runProgram?: boolean;
}

export interface PreviewOptions extends GlobalOpts {
    expectNoChanges?: boolean;
    diff?: boolean;
    replace?: string[];
    target?: string[];
    targetDependents?: boolean;
    exclude?: string[];
    excludeDependents?: boolean;
    /** Path to save the plan the preview proposes to (`pulumi preview --save-plan`). */
    plan?: string;
    program?: PulumiFn;
    refresh?: boolean;
    userAgent?: string;
    policyPacks?: string[];
    policyPackConfigs?: string[];
    importFile?: string;
    runProgram?: boolean;
}

export interface RefreshOptions extends GlobalOpts {
    expectNoChanges?: boolean;
    target?: string[];
    exclude?: string[];
    excludeDependents?: boolean;
    clearPendingCreates?: boolean;
    importPendingCreates?: { urn: string; id: string }[];
    program?: PulumiFn;
    runProgram?: boolean;
    skipPendingCreates?: boolean;
    previewOnly?: boolean;
    userAgent?: string;
}

export interface DestroyOptions extends GlobalOpts {
    target?: string[];
    targetDependents?: boolean;
    exclude?: string[];
    excludeDependents?: boolean;
    continueOnError?: boolean;
    excludeProtected?: boolean;
    previewOnly?: boolean;
    program?: PulumiFn;
    runProgram?: boolean;
    refresh?: boolean;
    userAgent?: string;
}

export interface UpResult {
    stdout: string;
    stderr: string;
    outputs: OutputMap;
    summary: UpdateSummary;
}
export interface PreviewResult {
    stdout: string;
    stderr: string;
    changeSummary: OpMap;
}
export interface RefreshResult {
    stdout: string;
    stderr: string;
    summary: UpdateSummary;
}
export interface DestroyResult {
    stdout: string;
    stderr: string;
    summary: UpdateSummary;
}

/** An inline program (the SDK's `PulumiFn`). */
export type PulumiFn = () => Promise<Record<string, unknown> | void> | Record<string, unknown> | void;

export interface ProjectRuntimeInfo {
    name: string;
    options?: { [key: string]: unknown };
}
export interface ProjectSettings {
    name: string;
    runtime?: ProjectRuntimeInfo | string;
    main?: string;
    description?: string;
    backend?: { url?: string };
    [other: string]: unknown;
}

export interface StackSettings {
    secretsProvider?: string;
    encryptedKey?: string;
    encryptionSalt?: string;
    config?: { [key: string]: string | { secure?: string; value?: unknown } };
}

export interface LocalWorkspaceOptions {
    workDir?: string;
    pulumiHome?: string;
    program?: PulumiFn;
    envVars?: { [key: string]: string };
    secretsProvider?: string;
    projectSettings?: ProjectSettings;
    stackSettings?: { [key: string]: StackSettings };
    pulumiCommand?: PulumiCommandLike;
    /** Remote workspaces are not supported; present so option bags type-check. */
    remote?: boolean;
    [other: string]: unknown;
}

export interface LocalProgramArgs {
    stackName: string;
    workDir: string;
}
export interface InlineProgramArgs {
    stackName: string;
    projectName: string;
    program: PulumiFn;
}

export interface RemoveOptions {
    force?: boolean;
}
export interface ListOptions {
    all?: boolean;
}

// ── Errors (the SDK's classes when given, else these) ────────────────────────

export interface CommandResultLike {
    stdout: string;
    stderr: string;
    code: number;
    err?: Error;
}

/** The pieces of `@pulumi/pulumi/automation` used to build SDK-identical errors. */
export interface AutomationSdkLike {
    CommandResult: new (stdout: string, stderr: string, code: number, err?: Error) => CommandResultLike;
    createCommandError: (result: CommandResultLike) => Error;
    CommandError: new (...args: never[]) => Error;
    ConcurrentUpdateError: new (...args: never[]) => Error;
    StackNotFoundError: new (...args: never[]) => Error;
    StackAlreadyExistsError: new (...args: never[]) => Error;
    [other: string]: unknown;
}

/** A structural copy of the SDK's `CommandResult`. */
export class CommandResult implements CommandResultLike {
    constructor(
        readonly stdout: string,
        readonly stderr: string,
        readonly code: number,
        readonly err?: Error,
    ) {}
    toString(): string {
        let errStr = "";
        if (this.err) {
            errStr = this.err.toString();
        }
        return `code: ${this.code}\n stdout: ${this.stdout}\n stderr: ${this.stderr}\n err? ${errStr}\n`;
    }
}

/** A structural copy of the SDK's `CommandError`. */
export class CommandError extends Error {
    constructor(readonly commandResult: CommandResultLike) {
        super(String(commandResult));
        this.name = "CommandError";
    }
}
export class ConcurrentUpdateError extends CommandError {
    constructor(result: CommandResultLike) {
        super(result);
        this.name = "ConcurrentUpdateError";
    }
}
export class StackNotFoundError extends CommandError {
    constructor(result: CommandResultLike) {
        super(result);
        this.name = "StackNotFoundError";
    }
}
export class StackAlreadyExistsError extends CommandError {
    constructor(result: CommandResultLike) {
        super(result);
        this.name = "StackAlreadyExistsError";
    }
}

const notFoundRegex = /no stack named.*found/;
const alreadyExistsRegex = /stack.*already exists/;
const conflictText = "[409] Conflict: Another update is currently in progress.";
const diyBackendConflictText = "the stack is currently locked by";

/** The SDK's classifier, over its stderr conventions. */
export function createCommandError(result: CommandResultLike): Error {
    const stderr = result.stderr;
    return notFoundRegex.test(stderr)
        ? new StackNotFoundError(result)
        : alreadyExistsRegex.test(stderr)
          ? new StackAlreadyExistsError(result)
          : stderr.indexOf(conflictText) >= 0 || stderr.indexOf(diyBackendConflictText) >= 0
            ? new ConcurrentUpdateError(result)
            : new CommandError(result);
}

const ownSdk: AutomationSdkLike = {
    CommandResult,
    createCommandError,
    CommandError,
    ConcurrentUpdateError,
    StackNotFoundError,
    StackAlreadyExistsError,
};

// ── Versions ─────────────────────────────────────────────────────────────────

/** What the SDK's `PulumiCommand.version` is: a `semver.SemVer` or a look-alike. */
export interface SemVerLike {
    major: number;
    minor: number;
    patch: number;
    version: string;
    compare(other: SemVerLike | string): -1 | 0 | 1;
    toString(): string;
}

function semverShim(version: string): SemVerLike {
    const m = /^v?(\d+)\.(\d+)\.(\d+)/.exec(version);
    const [major, minor, patch] = m ? [Number(m[1]), Number(m[2]), Number(m[3])] : [0, 0, 0];
    const cmp = (a: number, b: number): -1 | 0 | 1 => (a < b ? -1 : a > b ? 1 : 0);
    const self: SemVerLike = {
        major,
        minor,
        patch,
        version: `${major}.${minor}.${patch}`,
        compare(other) {
            const o = typeof other === "string" ? semverShim(other) : other;
            return cmp(major, o.major) || cmp(minor, o.minor) || cmp(patch, o.patch);
        },
        toString: () => self.version,
    };
    return self;
}

function parseSemVer(version: string): SemVerLike {
    try {
        // eslint-disable-next-line @typescript-eslint/no-require-imports
        const semver = require("semver") as { parse(v: string): SemVerLike | null };
        const parsed = semver.parse(version);
        if (parsed) {
            return parsed;
        }
    } catch {
        // semver is not a dependency of this package; the shim covers the SDK's uses.
    }
    return semverShim(version);
}

/** The Pulumi version embedded in libpulumi ("3.237.0"), from `version()`. */
export function embeddedPulumiVersion(): string {
    const m = /pulumi v(\d+\.\d+\.\d+[^)\s]*)/.exec(libraryVersion());
    return m ? m[1] : "0.0.0";
}

export interface PulumiCommandLike {
    command: string;
    version: SemVerLike | null;
    run?(...args: unknown[]): Promise<unknown>;
}

/**
 * Stands in for the SDK's `PulumiCommand`. `command` names the shared library
 * (nothing spawns it); `version` is the embedded Pulumi version, so SDK-side
 * minimum-version checks pass. `run` rejects: there is no CLI to run.
 */
export class PulumiCommand implements PulumiCommandLike {
    constructor(
        readonly command: string,
        readonly version: SemVerLike | null,
    ) {}

    static async get(_opts?: { root?: string; version?: unknown; skipVersionCheck?: boolean }): Promise<PulumiCommand> {
        return new PulumiCommand(libraryPath(), parseSemVer(embeddedPulumiVersion()));
    }

    static async install(_opts?: unknown): Promise<PulumiCommand> {
        return PulumiCommand.get();
    }

    async run(args: string[]): Promise<never> {
        throw new Error(
            `the in-process engine has no pulumi CLI to run (pulumi ${args.join(" ")}); ` +
                "use the Stack/Workspace methods instead",
        );
    }
}

// ── Pulumi.yaml (minimal, dependency-free) ───────────────────────────────────

function parseScalar(raw: string): string {
    const s = raw.trim();
    if ((s.startsWith('"') && s.endsWith('"')) || (s.startsWith("'") && s.endsWith("'"))) {
        try {
            return s.startsWith('"') ? (JSON.parse(s) as string) : s.slice(1, -1);
        } catch {
            return s.slice(1, -1);
        }
    }
    return s;
}

/**
 * Reads the keys the facade needs from a Pulumi.yaml (`name`, `runtime`,
 * `main`, `description`, `backend.url`) with js-yaml when it is resolvable and
 * a two-level line parser otherwise. The engine loads the full manifest itself.
 */
export function readProjectSettings(workDir: string): ProjectSettings | undefined {
    const file = path.join(workDir, "Pulumi.yaml");
    if (!fs.existsSync(file)) {
        return undefined;
    }
    const text = fs.readFileSync(file, "utf8");
    try {
        // eslint-disable-next-line @typescript-eslint/no-require-imports
        const yaml = require("js-yaml") as { load(s: string): unknown };
        const doc = yaml.load(text);
        if (doc && typeof doc === "object" && typeof (doc as ProjectSettings).name === "string") {
            return doc as ProjectSettings;
        }
    } catch {
        // fall through to the line parser
    }
    const out: Record<string, unknown> = {};
    let section: string | undefined;
    for (const line of text.split(/\r?\n/)) {
        if (!line.trim() || line.trim().startsWith("#")) {
            continue;
        }
        const top = /^(\w[\w-]*):\s*(.*)$/.exec(line);
        if (top) {
            section = top[1];
            if (top[2] !== "") {
                out[section] = parseScalar(top[2]);
                section = undefined;
            } else {
                out[section] = {};
            }
            continue;
        }
        const nested = /^\s+(\w[\w-]*):\s*(.*)$/.exec(line);
        if (nested && section && typeof out[section] === "object") {
            (out[section] as Record<string, unknown>)[nested[1]] = parseScalar(nested[2]);
        }
    }
    return typeof out.name === "string" ? (out as unknown as ProjectSettings) : undefined;
}

function writeProjectSettings(workDir: string, settings: ProjectSettings): void {
    const lines: string[] = [`name: ${settings.name}`];
    const runtime = settings.runtime ?? "nodejs";
    if (typeof runtime === "string") {
        lines.push(`runtime: ${runtime}`);
    } else {
        lines.push(`runtime:`, `  name: ${runtime.name}`);
        if (runtime.options && Object.keys(runtime.options).length > 0) {
            lines.push(`  options:`);
            for (const [k, v] of Object.entries(runtime.options)) {
                lines.push(`    ${k}: ${JSON.stringify(v)}`);
            }
        }
    }
    if (settings.main) {
        lines.push(`main: ${JSON.stringify(settings.main)}`);
    }
    if (settings.description) {
        lines.push(`description: ${JSON.stringify(settings.description)}`);
    }
    if (settings.backend?.url) {
        lines.push(`backend:`, `  url: ${settings.backend.url}`);
    }
    fs.writeFileSync(path.join(workDir, "Pulumi.yaml"), lines.join("\n") + "\n");
}

function isDIYBackend(url: string): boolean {
    return /^(file:|s3:|gs:|azblob:)/.test(url);
}

function runtimeName(settings: ProjectSettings | undefined): string {
    const r = settings?.runtime;
    return typeof r === "string" ? r : (r?.name ?? "nodejs");
}

// ── Rendering (what the CLI would print) ─────────────────────────────────────

const opSymbols: Record<string, string> = {
    same: " ",
    create: "+",
    update: "~",
    delete: "-",
    replace: "+-",
    "create-replacement": "++",
    "delete-replaced": "--",
    read: ">",
    "read-replacement": ">>",
    refresh: "~",
    "discard": "<",
    "discard-replaced": "<<",
    "remove-pending-replace": "~",
    import: "=",
    "import-replacement": "=>",
};

const opVerbs: Record<string, [string, string]> = {
    same: ["", ""],
    create: ["creating", "created"],
    update: ["updating", "updated"],
    delete: ["deleting", "deleted"],
    replace: ["replacing", "replaced"],
    "create-replacement": ["creating replacement", "created replacement"],
    "delete-replaced": ["deleting original", "deleted original"],
    read: ["reading", "read"],
    "read-replacement": ["reading for replacement", "read for replacement"],
    refresh: ["refreshing", "refreshed"],
    discard: ["discarding", "discarded"],
    "discard-replaced": ["discarding original", "discarded original"],
    "remove-pending-replace": ["removing pending replace", "removed pending replace"],
    import: ["importing", "imported"],
    "import-replacement": ["importing replacement", "imported replacement"],
};

const opPastTense: Record<string, string> = {
    create: "created",
    update: "updated",
    delete: "deleted",
    replace: "replaced",
    same: "unchanged",
    read: "read",
    refresh: "refreshed",
    discard: "discarded",
    import: "imported",
};

function urnName(urn: string): string {
    return urn.slice(urn.lastIndexOf("::") + 2);
}

function headerLine(kind: EngineResult["kind"], dryRun: boolean, stackName: string): string {
    switch (kind) {
        case "preview":
            return `Previewing update (${stackName}):`;
        case "up":
            return `Updating (${stackName}):`;
        case "refresh":
            return dryRun ? `Previewing refresh (${stackName}):` : `Refreshing (${stackName}):`;
        case "destroy":
            return dryRun ? `Previewing destroy (${stackName}):` : `Destroying (${stackName}):`;
    }
}

function renderStep(metadata: StepEventMetadata, done: boolean): string | undefined {
    if (metadata.op === "same" && !done) {
        return undefined;
    }
    const sym = opSymbols[metadata.op] ?? metadata.op;
    const verbs = opVerbs[metadata.op];
    const verb = verbs ? (done ? verbs[1] : verbs[0]) : done ? metadata.op : metadata.op + "…";
    const name = urnName(metadata.urn);
    return ` ${sym.padEnd(2)} ${metadata.type} ${name} ${verb}`.trimEnd();
}

function renderSummary(summary: SummaryEvent): string {
    const lines = ["", "Resources:"];
    const changes = summary.resourceChanges ?? {};
    for (const [op, count] of Object.entries(changes)) {
        if (!count) {
            continue;
        }
        const sym = opSymbols[op] ?? " ";
        lines.push(`    ${sym.padEnd(2)} ${count} ${opPastTense[op] ?? op}`);
    }
    lines.push("", `Duration: ${summary.durationSeconds}s`, "");
    return lines.join("\n");
}

// ── Option and result mapping ────────────────────────────────────────────────

function changeCount(changes: OpMap | undefined): number {
    return Object.entries(changes ?? {}).reduce(
        (total, [op, count]) => (op === "same" || op === "read" || op === "refresh" ? total : total + (count ?? 0)),
        0,
    );
}

function engineOptions(kind: EngineResult["kind"], opts: GlobalOpts & Record<string, unknown>, dryRun: boolean): EngineOptions {
    const out: EngineOptions = {};
    if (typeof opts.parallel === "number") {
        out.parallel = opts.parallel;
    }
    if (typeof opts.message === "string") {
        out.message = opts.message;
    }
    if (Array.isArray(opts.target)) {
        out.targets = opts.target as string[];
    }
    if (opts.targetDependents === true) {
        out.targetDependents = true;
    }
    if (Array.isArray(opts.exclude)) {
        out.excludes = opts.exclude as string[];
    }
    if (Array.isArray(opts.replace)) {
        out.replaces = opts.replace as string[];
    }
    if (opts.refresh === true) {
        out.refresh = true;
    }
    if (opts.continueOnError === true) {
        out.continueOnError = true;
    }
    if (opts.showSecrets !== undefined) {
        out.showSecrets = Boolean(opts.showSecrets);
    }
    // The SDK's `plan` option is `--save-plan` on preview and `--plan` on up;
    // the engine has one field for each.
    if (typeof opts.plan === "string" && opts.plan !== "") {
        if (kind === "preview") {
            out.savePlan = opts.plan;
        } else {
            out.plan = opts.plan;
        }
    }
    if (dryRun) {
        out.dryRun = true;
    }
    if (opts.env && typeof opts.env === "object") {
        out.env = opts.env as Record<string, string>;
    }
    if (opts.signal) {
        out.signal = opts.signal;
    }
    return out;
}

/** Strip the library's `type` discriminator: the SDK's event has none. */
function toEngineEvent(e: EngineEvent_): EngineEvent {
    const { type: _type, ...rest } = e;
    return rest as unknown as EngineEvent;
}

function outputMap(outputs: EngineResult["outputs"] | { values: Record<string, unknown>; secretKeys: string[] }): OutputMap {
    const out: OutputMap = {};
    if (!outputs) {
        return out;
    }
    const secrets = new Set(outputs.secretKeys ?? []);
    for (const [k, v] of Object.entries(outputs.values ?? {})) {
        out[k] = { value: v, secret: secrets.has(k) };
    }
    return out;
}

const updateKinds = new Set<string>(["update", "preview", "refresh", "rename", "destroy", "import"]);
const updateResults = new Set<string>(["not-started", "in-progress", "succeeded", "failed"]);

/** The SDK's `UpdateSummary` from the library's `UpdateInfo`. */
function updateSummary(info: EngineUpdateInfo): UpdateSummary {
    const config: ConfigMap = {};
    for (const [k, v] of Object.entries(info.config ?? {})) {
        config[k] = { value: v.value, secret: v.secret === true };
    }
    return {
        kind: (updateKinds.has(info.kind) ? info.kind : "update") as UpdateKind,
        result: (updateResults.has(info.result) ? info.result : "succeeded") as UpdateResult,
        message: info.message ?? "",
        startTime: new Date(info.startTime * 1000),
        endTime: new Date(info.endTime * 1000),
        version: info.version ?? 0,
        environment: info.environment ?? {},
        config,
        ...(info.resourceChanges ? { resourceChanges: info.resourceChanges as OpMap } : {}),
    };
}

interface OperationOutcome {
    stdout: string;
    stderr: string;
    summary: SummaryEvent | undefined;
    result: EngineResult;
    startedAt: Date;
    endedAt: Date;
}

// ── Workspace ────────────────────────────────────────────────────────────────

/**
 * Internal state shared by a workspace and the stacks opened through it:
 * one engine handle per stack name. Update history lives in the backend.
 */
class WorkspaceState {
    readonly handles = new Map<string, EngineStack>();
}

/**
 * The SDK's `LocalWorkspace`, over the library. A workspace is a directory
 * holding `Pulumi.yaml` (and the `Pulumi.<stack>.yaml` files the engine
 * writes), an env-var map, a secrets provider, and optionally an inline
 * program. Stacks are opened on the backend the env/`Pulumi.yaml` name.
 */
export class LocalWorkspace {
    readonly workDir: string;
    readonly pulumiHome: string | undefined;
    readonly secretsProvider: string | undefined;
    readonly envVars: { [key: string]: string };
    readonly pulumiCommand: PulumiCommandLike;
    program: PulumiFn | undefined;
    readonly pulumiVersion: string;

    /** @internal */
    readonly state = new WorkspaceState();
    private readonly stackSettingsIn: { [key: string]: StackSettings };
    private readonly sdk: AutomationSdkLike;
    private currentStack: string | undefined;

    constructor(opts: LocalWorkspaceOptions = {}, sdk: AutomationSdkLike = ownSdk) {
        this.sdk = sdk;
        this.workDir = opts.workDir ?? fs.mkdtempSync(path.join(os.tmpdir(), "automation-"));
        fs.mkdirSync(this.workDir, { recursive: true });
        this.pulumiHome = opts.pulumiHome;
        this.secretsProvider = opts.secretsProvider;
        this.envVars = { ...(opts.envVars ?? {}) };
        this.program = opts.program;
        this.stackSettingsIn = { ...(opts.stackSettings ?? {}) };
        this.pulumiCommand = opts.pulumiCommand ?? new PulumiCommand(libraryPath(), parseSemVer(embeddedPulumiVersion()));
        this.pulumiVersion = embeddedPulumiVersion();
        if (opts.projectSettings) {
            writeProjectSettings(this.workDir, opts.projectSettings);
        }
    }

    // ── The SDK's static openers ──

    static async create(opts?: LocalWorkspaceOptions): Promise<LocalWorkspace> {
        return new LocalWorkspace(opts);
    }

    static async createStack(args: InlineProgramArgs | LocalProgramArgs, opts?: LocalWorkspaceOptions): Promise<Stack> {
        return LocalWorkspace.open("create", args, opts, ownSdk);
    }

    static async selectStack(args: InlineProgramArgs | LocalProgramArgs, opts?: LocalWorkspaceOptions): Promise<Stack> {
        return LocalWorkspace.open("select", args, opts, ownSdk);
    }

    static async createOrSelectStack(
        args: InlineProgramArgs | LocalProgramArgs,
        opts?: LocalWorkspaceOptions,
    ): Promise<Stack> {
        return LocalWorkspace.open("createOrSelect", args, opts, ownSdk);
    }

    /** @internal Shared by the statics and the SDK-bound module. */
    static async open(
        mode: "create" | "select" | "createOrSelect",
        args: InlineProgramArgs | LocalProgramArgs,
        opts: LocalWorkspaceOptions | undefined,
        sdk: AutomationSdkLike,
    ): Promise<Stack> {
        const wsOpts: LocalWorkspaceOptions = { ...opts };
        if ("workDir" in args && args.workDir) {
            wsOpts.workDir = args.workDir;
        }
        if ("program" in args) {
            wsOpts.program = args.program;
            const workDir = wsOpts.workDir ?? fs.mkdtempSync(path.join(os.tmpdir(), "automation-"));
            wsOpts.workDir = workDir;
            if (!wsOpts.projectSettings && !readProjectSettings(workDir)) {
                wsOpts.projectSettings = { name: args.projectName, runtime: "nodejs" };
            }
        }
        const ws = new LocalWorkspace(wsOpts, sdk);
        switch (mode) {
            case "create":
                return Stack.create(args.stackName, ws);
            case "select":
                return Stack.select(args.stackName, ws);
            case "createOrSelect":
                return Stack.createOrSelect(args.stackName, ws);
        }
    }

    // ── Settings ──

    async projectSettings(): Promise<ProjectSettings> {
        const settings = readProjectSettings(this.workDir);
        if (!settings) {
            throw new Error(`no Pulumi.yaml project file found in ${this.workDir}`);
        }
        return settings;
    }

    async saveProjectSettings(settings: ProjectSettings): Promise<void> {
        writeProjectSettings(this.workDir, settings);
    }

    async stackSettings(stackName: string): Promise<StackSettings> {
        const file = path.join(this.workDir, `Pulumi.${stackName}.yaml`);
        const out: StackSettings = { ...(this.stackSettingsIn[stackName] ?? {}) };
        if (fs.existsSync(file)) {
            for (const line of fs.readFileSync(file, "utf8").split(/\r?\n/)) {
                const m = /^(secretsprovider|encryptionsalt|encryptedkey):\s*(.*)$/.exec(line);
                if (m) {
                    const key = { secretsprovider: "secretsProvider", encryptionsalt: "encryptionSalt", encryptedkey: "encryptedKey" }[m[1]];
                    (out as Record<string, unknown>)[key!] = parseScalar(m[2]);
                }
            }
        }
        return out;
    }

    async saveStackSettings(stackName: string, settings: StackSettings): Promise<void> {
        this.stackSettingsIn[stackName] = settings;
    }

    /** The backend this workspace addresses: env, then Pulumi.yaml, then PULUMI_BACKEND_URL. */
    backendUrl(): string {
        const url =
            this.envVars.PULUMI_BACKEND_URL ??
            process.env.PULUMI_BACKEND_URL ??
            readProjectSettings(this.workDir)?.backend?.url;
        if (!url) {
            throw new Error(
                `no backend for workspace ${this.workDir}: set PULUMI_BACKEND_URL (envVars or process.env) or backend.url in Pulumi.yaml`,
            );
        }
        return url;
    }

    private env(key: string): string | undefined {
        return this.envVars[key] ?? process.env[key];
    }

    private passphrase(): string | undefined {
        const direct = this.env("PULUMI_CONFIG_PASSPHRASE");
        if (direct !== undefined) {
            return direct;
        }
        const file = this.env("PULUMI_CONFIG_PASSPHRASE_FILE");
        if (file) {
            return fs.readFileSync(file, "utf8").trim();
        }
        return undefined;
    }

    /** @internal */
    stackSpec(stackName: string, create: boolean): StackSpec {
        const settings = readProjectSettings(this.workDir);
        if (!settings) {
            throw this.commandError("", "error: no Pulumi.yaml project file found\n", 255);
        }
        const backendUrl = this.backendUrl();
        const stackSettings = this.stackSettingsIn[stackName];
        let provider = stackSettings?.secretsProvider ?? this.secretsProvider;
        if (!provider || provider === "default") {
            provider = isDIYBackend(backendUrl) ? "passphrase" : "service";
        }
        const secrets: StackSpec["secrets"] = { provider };
        if (provider === "passphrase") {
            const passphrase = this.passphrase();
            // `PULUMI_CONFIG_PASSPHRASE=""` is a valid CLI passphrase, and the
            // library takes the key's presence as the confirmation; only an
            // unset passphrase (no env var, no _FILE) is an error.
            if (passphrase === undefined) {
                throw new Error(
                    "the passphrase secrets provider needs PULUMI_CONFIG_PASSPHRASE " +
                        "(or PULUMI_CONFIG_PASSPHRASE_FILE) in the workspace's envVars or the process environment",
                );
            }
            secrets.passphrase = passphrase;
        }
        const config: Record<string, EngineConfigValue> = {};
        for (const [k, v] of Object.entries(stackSettings?.config ?? {})) {
            if (typeof v === "string") {
                config[k] = { value: v };
            } else if (v && typeof v.secure === "string") {
                config[k] = { value: v.secure, secret: true };
            } else if (v && v.value !== undefined) {
                config[k] = typeof v.value === "string" ? { value: v.value } : { value: JSON.stringify(v.value), object: true };
            }
        }
        const token = this.env("PULUMI_ACCESS_TOKEN");
        // The SDK hands `envVars` to the `pulumi` process, whose plugins and
        // language hosts inherit them; the library takes the same map as the
        // stack's plugin environment (it never touches this process's env).
        const env: Record<string, string> = { ...this.envVars };
        if (this.pulumiHome && env.PULUMI_HOME === undefined) {
            env.PULUMI_HOME = this.pulumiHome;
        }
        return {
            name: stackName,
            project: { name: settings.name, dir: this.workDir, description: settings.description },
            backend: { url: backendUrl, ...(token && !isDIYBackend(backendUrl) ? { token } : {}) },
            secrets,
            ...(Object.keys(config).length > 0 ? { config } : {}),
            ...(Object.keys(env).length > 0 ? { env } : {}),
            create,
        };
    }

    /** @internal */
    commandError(stdout: string, stderr: string, code: number, cause?: unknown): Error {
        const err = this.sdk.createCommandError(new this.sdk.CommandResult(stdout, stderr, code));
        if (cause !== undefined) {
            (err as { cause?: unknown }).cause = cause;
        }
        return err;
    }

    /** @internal Translate a library error into the SDK's error taxonomy. */
    translate(error: unknown, stackName: string, stdout = "", stderr = ""): Error {
        if (error instanceof EngineStackNotFoundError) {
            return this.commandError(stdout, `${stderr}error: no stack named '${stackName}' found\n`, 255, error);
        }
        if (error instanceof StackExistsError) {
            return this.commandError(stdout, `${stderr}error: stack '${stackName}' already exists\n`, 255, error);
        }
        if (error instanceof EngineConcurrentUpdateError) {
            return this.commandError(stdout, `${stderr}error: ${conflictText}\n${error.message}\n`, 255, error);
        }
        if (error instanceof UnsupportedError) {
            return this.commandError(stdout, `${stderr}error: ${error.message}\n`, 255, error);
        }
        if (error instanceof CancelledError) {
            const what = /preview/i.test(error.operation) ? "preview" : "update";
            return this.commandError(stdout, `${stderr}error: ${what} canceled\n`, 1, error);
        }
        if (error instanceof PulumiError) {
            return this.commandError(stdout, `${stderr}error: ${error.message}\n`, 255, error);
        }
        if (error instanceof Error) {
            return error;
        }
        return new Error(String(error));
    }

    /** @internal Open (once per name) the engine handle for a stack. */
    async engineStack(stackName: string, create: boolean): Promise<EngineStack> {
        const cached = this.state.handles.get(stackName);
        if (cached) {
            return cached;
        }
        let handle: EngineStack;
        try {
            handle = await openStack(this.stackSpec(stackName, create));
        } catch (error) {
            throw this.translate(error, stackName);
        }
        this.state.handles.set(stackName, handle);
        this.currentStack = stackName;
        return handle;
    }

    // ── The SDK's Workspace stack methods ──

    async createStack(stackName: string): Promise<void> {
        // The library's `create` opens an existing stack silently; the SDK's
        // createStack must fail on one, so probe first.
        let exists = true;
        try {
            const probe = await openStack(this.stackSpec(stackName, false));
            probe.close();
        } catch (error) {
            if (error instanceof EngineStackNotFoundError) {
                exists = false;
            } else {
                throw this.translate(error, stackName);
            }
        }
        if (exists) {
            throw this.translate(new StackExistsError(stackName, `stack '${stackName}' already exists`), stackName);
        }
        await this.engineStack(stackName, true);
    }

    async selectStack(stackName: string): Promise<void> {
        await this.engineStack(stackName, false);
    }

    async removeStack(stackName: string, opts?: RemoveOptions): Promise<void> {
        const handle = await this.engineStack(stackName, false);
        try {
            handle.remove(opts?.force === true);
        } catch (error) {
            throw this.translate(error, stackName);
        }
        handle.close();
        this.state.handles.delete(stackName);
        if (this.currentStack === stackName) {
            this.currentStack = undefined;
        }
    }

    /**
     * The stacks of this workspace's backend, from the library's listing
     * (every backend, not just `file://`). `name` is the elided reference
     * `pulumi stack ls` prints.
     */
    async listStacks(_opts?: ListOptions): Promise<StackSummary[]> {
        const url = this.backendUrl();
        const token = this.env("PULUMI_ACCESS_TOKEN");
        const project = readProjectSettings(this.workDir)?.name;
        let listed;
        try {
            listed = libraryListStacks(
                { url, ...(token && !isDIYBackend(url) ? { token } : {}) },
                project ? { project } : {},
            );
        } catch (error) {
            throw this.translate(error, this.currentStack ?? "");
        }
        return listed.map((s) => ({
            name: s.name,
            current: s.name === this.currentStack || s.fullName === this.currentStack,
            ...(s.lastUpdate ? { lastUpdate: new Date(s.lastUpdate).toISOString() } : {}),
            ...(s.resourceCount !== undefined ? { resourceCount: s.resourceCount } : {}),
        }));
    }

    async stack(): Promise<StackSummary | undefined> {
        if (!this.currentStack) {
            return undefined;
        }
        return { name: this.currentStack, current: true };
    }

    async exportStack(stackName: string): Promise<Deployment> {
        const handle = await this.engineStack(stackName, false);
        try {
            return handle.export() as Deployment;
        } catch (error) {
            throw this.translate(error, stackName);
        }
    }

    async importStack(stackName: string, state: Deployment): Promise<void> {
        const handle = await this.engineStack(stackName, false);
        try {
            handle.import(state);
        } catch (error) {
            throw this.translate(error, stackName);
        }
    }

    async stackOutputs(stackName: string): Promise<OutputMap> {
        const handle = await this.engineStack(stackName, false);
        try {
            return outputMap(handle.outputs(true));
        } catch (error) {
            throw this.translate(error, stackName);
        }
    }

    async getConfig(stackName: string, key: string): Promise<ConfigValue> {
        const handle = await this.engineStack(stackName, false);
        const v = handle.getConfig(key);
        if (v === undefined) {
            throw this.commandError("", `error: configuration key '${key}' not found for stack '${stackName}'\n`, 255);
        }
        return { value: v.value, secret: v.secret === true };
    }

    async getAllConfig(stackName: string): Promise<ConfigMap> {
        const handle = await this.engineStack(stackName, false);
        const settings = readProjectSettings(this.workDir);
        const out: ConfigMap = {};
        // The library reads keys one at a time; the stack file names them.
        const file = path.join(this.workDir, `Pulumi.${stackName}.yaml`);
        if (fs.existsSync(file)) {
            let inConfig = false;
            for (const line of fs.readFileSync(file, "utf8").split(/\r?\n/)) {
                if (/^config:/.test(line)) {
                    inConfig = true;
                    continue;
                }
                if (inConfig && /^\S/.test(line)) {
                    inConfig = false;
                }
                const m = inConfig ? /^ {2}([^\s:]+):/.exec(line) : null;
                if (m) {
                    const v = handle.getConfig(m[1]);
                    if (v) {
                        const key = m[1].includes(":") ? m[1] : `${settings?.name ?? ""}:${m[1]}`;
                        out[key] = { value: v.value, secret: v.secret === true };
                    }
                }
            }
        }
        return out;
    }

    async setConfig(stackName: string, key: string, value: ConfigValue): Promise<void> {
        const handle = await this.engineStack(stackName, false);
        handle.setConfig(key, value.secret ? new Secret(value.value) : value.value);
    }

    async setAllConfig(stackName: string, config: ConfigMap): Promise<void> {
        for (const [k, v] of Object.entries(config)) {
            await this.setConfig(stackName, k, v);
        }
    }

    async removeConfig(stackName: string, key: string): Promise<void> {
        const handle = await this.engineStack(stackName, false);
        // The library has no delete; clear the value.
        handle.setConfig(key, "");
    }

    async refreshConfig(stackName: string): Promise<ConfigMap> {
        return this.getAllConfig(stackName);
    }

    async whoAmI(): Promise<{ user: string; url?: string; organizations?: string[] }> {
        return { user: os.userInfo().username, url: this.backendUrl() };
    }

    /** Release every engine handle this workspace opened. Not part of the SDK. */
    close(): void {
        for (const h of this.state.handles.values()) {
            h.close();
        }
        this.state.handles.clear();
    }
}

// ── Stack ────────────────────────────────────────────────────────────────────

/**
 * The SDK's `Stack`: lifecycle operations on one stack of a workspace, with
 * the SDK's option bags, callbacks, results and errors.
 */
export class Stack {
    private constructor(
        readonly name: string,
        readonly workspace: LocalWorkspace,
        private readonly sdk: AutomationSdkLike,
    ) {}

    static async create(name: string, workspace: LocalWorkspace): Promise<Stack> {
        await workspace.createStack(name);
        return new Stack(name, workspace, workspace["sdk"]);
    }

    static async select(name: string, workspace: LocalWorkspace): Promise<Stack> {
        await workspace.selectStack(name);
        return new Stack(name, workspace, workspace["sdk"]);
    }

    static async createOrSelect(name: string, workspace: LocalWorkspace): Promise<Stack> {
        await workspace.engineStack(name, true);
        return new Stack(name, workspace, workspace["sdk"]);
    }

    private program(opts: { program?: PulumiFn; runProgram?: boolean }, required: boolean): Program | undefined {
        const inline = opts.program ?? this.workspace.program;
        if (inline) {
            return inline as unknown as InlineProgram;
        }
        if (!required && opts.runProgram !== true) {
            return undefined;
        }
        return { mode: "local", dir: this.workspace.workDir };
    }

    async up(opts: UpOptions = {}): Promise<UpResult> {
        const outcome = await this.run("up", this.program(opts, true), opts, false);
        const summary = await this.latestSummary("update", outcome, opts.message);
        // The SDK's UpResult.outputs shows secret values (`stack output --show-secrets`).
        const outputs = await this.workspace.stackOutputs(this.name);
        return { stdout: outcome.stdout, stderr: outcome.stderr, summary, outputs };
    }

    async preview(opts: PreviewOptions = {}): Promise<PreviewResult> {
        const outcome = await this.run("preview", this.program(opts, true), opts, false);
        return { stdout: outcome.stdout, stderr: outcome.stderr, changeSummary: outcome.summary?.resourceChanges ?? {} };
    }

    async refresh(opts: RefreshOptions = {}): Promise<RefreshResult> {
        const dryRun = opts.previewOnly === true;
        const outcome = await this.run("refresh", this.program(opts, false), opts, dryRun);
        const summary = await this.latestSummary("refresh", outcome, opts.message, !dryRun);
        return { stdout: outcome.stdout, stderr: outcome.stderr, summary };
    }

    async destroy(opts: DestroyOptions = {}): Promise<DestroyResult> {
        const dryRun = opts.previewOnly === true;
        const outcome = await this.run("destroy", this.program(opts, false), opts, dryRun);
        const summary = await this.latestSummary("destroy", outcome, opts.message, !dryRun);
        return { stdout: outcome.stdout, stderr: outcome.stderr, summary };
    }

    async previewDestroy(opts: DestroyOptions = {}): Promise<PreviewResult> {
        const outcome = await this.run("destroy", this.program(opts, false), opts, true);
        return { stdout: outcome.stdout, stderr: outcome.stderr, changeSummary: outcome.summary?.resourceChanges ?? {} };
    }

    /**
     * The `UpdateSummary` the SDK returns from an operation: the newest
     * history entry the backend recorded for it. A dry run (`previewOnly`)
     * records nothing, so the outcome is described directly.
     */
    private async latestSummary(
        kind: UpdateKind,
        outcome: OperationOutcome,
        message: string | undefined,
        recorded = true,
    ): Promise<UpdateSummary> {
        const newest = recorded ? (await this.history(1, 1)).at(0) : undefined;
        if (newest) {
            return newest;
        }
        return {
            kind,
            startTime: outcome.startedAt,
            endTime: outcome.endedAt,
            message: message ?? "",
            environment: {},
            config: {},
            result: "succeeded",
            version: 0,
            ...(outcome.summary ? { resourceChanges: outcome.summary.resourceChanges } : {}),
        };
    }

    /**
     * Run one engine operation: stream its events to `onEvent`, render text
     * to `onOutput`/`onError`, then settle as the SDK would — a result, or a
     * `CommandError` (subclass) whose `cause` is the library's typed error.
     */
    private async run(
        kind: EngineResult["kind"],
        program: Program | undefined,
        opts: GlobalOpts & { expectNoChanges?: boolean },
        dryRun: boolean,
    ): Promise<OperationOutcome> {
        opts.signal?.throwIfAborted();
        const handle = await this.workspace.engineStack(this.name, false);
        const startedAt = new Date();
        let stdout = "";
        let stderr = "";
        const out = (text: string): void => {
            stdout += text;
            opts.onOutput?.(text);
        };
        const err = (text: string): void => {
            stderr += text;
            opts.onError?.(text);
        };
        let summary: SummaryEvent | undefined;
        const isPreview = kind === "preview" || dryRun;

        let operation;
        try {
            operation = await (program === undefined
                ? (handle as unknown as { [k: string]: (p: undefined, o: EngineOptions) => Promise<unknown> })[kind](
                      undefined,
                      engineOptions(kind, opts, dryRun),
                  )
                : (handle as unknown as { [k: string]: (p: Program, o: EngineOptions) => Promise<unknown> })[kind](
                      program,
                      engineOptions(kind, opts, dryRun),
                  ));
        } catch (error) {
            throw this.workspace.translate(error, this.name);
        }
        const op = operation as import("./index").Operation;
        // The banner the DIY backend writes arrives as a stdout event; the
        // facade has already rendered the same line, so it is not written
        // twice. Events carry the library's own sequence and timestamp, and
        // the stream ends with the `cancelEvent` marker, so nothing here is
        // synthesised: they are forwarded as they come.
        const header = headerLine(kind, dryRun, this.name);
        out(header + "\n");
        const emit = (event: EngineEvent): void => {
            opts.onEvent?.(event);
        };
        try {
            for await (const raw of op) {
                const event = toEngineEvent(raw);
                if (event.diagnosticEvent) {
                    const d = event.diagnosticEvent;
                    if (d.severity !== "debug" && !d.ephemeral) {
                        const line = `${d.prefix ?? ""}${d.message}`;
                        out(line.endsWith("\n") ? line : line + "\n");
                    }
                } else if (event.resourcePreEvent) {
                    const line = renderStep(event.resourcePreEvent.metadata, false);
                    if (line) {
                        out(line + "\n");
                    }
                } else if (event.resOutputsEvent) {
                    const line = renderStep(event.resOutputsEvent.metadata, true);
                    if (line) {
                        out(line + "\n");
                    }
                } else if (event.resOpFailedEvent) {
                    const m = event.resOpFailedEvent.metadata;
                    out(` ${(opSymbols[m.op] ?? m.op).padEnd(2)} ${m.type} ${urnName(m.urn)} **${m.op} failed**\n`);
                } else if (event.summaryEvent) {
                    summary = event.summaryEvent;
                    out(renderSummary(summary));
                } else if (event.stdoutEvent) {
                    if (!event.stdoutEvent.message.startsWith(header)) {
                        out(event.stdoutEvent.message);
                    }
                }
                emit(event);
            }
            let result: EngineResult;
            try {
                result = await op.result();
            } catch (error) {
                if (error instanceof CancelledError) {
                    out(isPreview ? "Preview canceled\n" : "Update canceled\n");
                }
                const failure = this.workspace.translate(error, this.name, stdout, stderr);
                err(`${failure.message}\n`);
                throw failure;
            }
            if (opts.expectNoChanges === true) {
                const changes = changeCount(summary?.resourceChanges);
                if (changes > 0) {
                    const message = `error: no changes were expected but ${changes} changes occurred\n`;
                    err(message);
                    throw this.workspace.commandError(stdout, stderr, 255);
                }
            }
            return { stdout, stderr, summary, result, startedAt, endedAt: new Date() };
        } finally {
            await op.release();
        }
    }

    // ── Stack state ──

    async cancel(): Promise<void> {
        const handle = await this.workspace.engineStack(this.name, false);
        try {
            handle.cancel();
        } catch (error) {
            throw this.workspace.translate(error, this.name);
        }
    }

    async outputs(): Promise<OutputMap> {
        return this.workspace.stackOutputs(this.name);
    }

    async exportStack(): Promise<Deployment> {
        return this.workspace.exportStack(this.name);
    }

    async importStack(state: Deployment): Promise<void> {
        return this.workspace.importStack(this.name, state);
    }

    /** The stack's newest update, from the backend's history. */
    async info(): Promise<UpdateSummary | undefined> {
        return (await this.history(1, 1)).at(0);
    }

    /**
     * The backend's update history, newest first. Previews are never
     * recorded; the DIY backend does not number its updates and reports
     * version 0.
     */
    async history(pageSize?: number, page?: number, showSecrets?: boolean): Promise<UpdateSummary[]> {
        const handle = await this.workspace.engineStack(this.name, false);
        let entries: EngineUpdateInfo[];
        try {
            entries = handle.history({
                ...(pageSize !== undefined ? { limit: pageSize } : {}),
                ...(page !== undefined ? { page } : {}),
                ...(showSecrets !== undefined ? { showSecrets } : {}),
            });
        } catch (error) {
            throw this.workspace.translate(error, this.name);
        }
        return entries.map(updateSummary);
    }

    private async tags(): Promise<{ [key: string]: string }> {
        const handle = await this.workspace.engineStack(this.name, false);
        try {
            return handle.getTags();
        } catch (error) {
            throw this.workspace.translate(error, this.name);
        }
    }

    private async writeTags(tags: { [key: string]: string }): Promise<void> {
        const handle = await this.workspace.engineStack(this.name, false);
        try {
            handle.setTags(tags);
        } catch (error) {
            throw this.workspace.translate(error, this.name);
        }
    }

    async listTags(): Promise<{ [key: string]: string }> {
        return this.tags();
    }

    async getTag(key: string): Promise<string> {
        const tags = await this.tags();
        const value = tags[key];
        if (value === undefined) {
            throw this.workspace.commandError("", `error: no tag named '${key}' found for stack '${this.name}'\n`, 255);
        }
        return value;
    }

    async setTag(key: string, value: string): Promise<void> {
        const tags = await this.tags();
        tags[key] = value;
        await this.writeTags(tags);
    }

    async removeTag(key: string): Promise<void> {
        const tags = await this.tags();
        delete tags[key];
        await this.writeTags(tags);
    }

    async getConfig(key: string): Promise<ConfigValue> {
        return this.workspace.getConfig(this.name, key);
    }
    async getAllConfig(): Promise<ConfigMap> {
        return this.workspace.getAllConfig(this.name);
    }
    async setConfig(key: string, value: ConfigValue): Promise<void> {
        return this.workspace.setConfig(this.name, key, value);
    }
    async setAllConfig(config: ConfigMap): Promise<void> {
        return this.workspace.setAllConfig(this.name, config);
    }
    async removeConfig(key: string): Promise<void> {
        return this.workspace.removeConfig(this.name, key);
    }
    async refreshConfig(): Promise<ConfigMap> {
        return this.workspace.refreshConfig(this.name);
    }

    /** The SDK's `Stack` has no close; the engine handle needs one. */
    close(): void {
        const h = this.workspace.state.handles.get(this.name);
        if (h) {
            h.close();
            this.workspace.state.handles.delete(this.name);
        }
    }
}

// ── Module assembly ──────────────────────────────────────────────────────────

/** The `@pulumi/pulumi/automation` names this facade provides. */
export interface AutomationCompatModule {
    LocalWorkspace: typeof LocalWorkspace;
    Stack: typeof Stack;
    PulumiCommand: typeof PulumiCommand;
    CommandResult: AutomationSdkLike["CommandResult"];
    createCommandError: AutomationSdkLike["createCommandError"];
    CommandError: AutomationSdkLike["CommandError"];
    ConcurrentUpdateError: AutomationSdkLike["ConcurrentUpdateError"];
    StackNotFoundError: AutomationSdkLike["StackNotFoundError"];
    StackAlreadyExistsError: AutomationSdkLike["StackAlreadyExistsError"];
    fullyQualifiedStackName(org: string, project: string, stack: string): string;
    [other: string]: unknown;
}

export function fullyQualifiedStackName(org: string, project: string, stack: string): string {
    return `${org}/${project}/${stack}`;
}

/**
 * Build the module a consumer installs in place of `@pulumi/pulumi/automation`.
 * With `sdk` (the consumer's own `@pulumi/pulumi/automation` instance), errors
 * are that SDK's classes and every other SDK export is passed through, so the
 * consumer's `instanceof` checks and unrelated helpers keep working; the
 * facade replaces `LocalWorkspace`, `Stack` and `PulumiCommand`.
 */
export function createAutomationModule(options: { sdk?: AutomationSdkLike } = {}): AutomationCompatModule {
    const sdk = options.sdk ?? ownSdk;
    class BoundLocalWorkspace extends LocalWorkspace {
        constructor(opts: LocalWorkspaceOptions = {}) {
            super(opts, sdk);
        }
        static override async create(opts?: LocalWorkspaceOptions): Promise<LocalWorkspace> {
            return new BoundLocalWorkspace(opts);
        }
        static override async createStack(args: InlineProgramArgs | LocalProgramArgs, opts?: LocalWorkspaceOptions): Promise<Stack> {
            return LocalWorkspace.open("create", args, opts, sdk);
        }
        static override async selectStack(args: InlineProgramArgs | LocalProgramArgs, opts?: LocalWorkspaceOptions): Promise<Stack> {
            return LocalWorkspace.open("select", args, opts, sdk);
        }
        static override async createOrSelectStack(
            args: InlineProgramArgs | LocalProgramArgs,
            opts?: LocalWorkspaceOptions,
        ): Promise<Stack> {
            return LocalWorkspace.open("createOrSelect", args, opts, sdk);
        }
    }
    return {
        ...(options.sdk ?? {}),
        LocalWorkspace: BoundLocalWorkspace as typeof LocalWorkspace,
        Stack,
        PulumiCommand,
        CommandResult: sdk.CommandResult,
        createCommandError: sdk.createCommandError,
        CommandError: sdk.CommandError,
        ConcurrentUpdateError: sdk.ConcurrentUpdateError,
        StackNotFoundError: sdk.StackNotFoundError,
        StackAlreadyExistsError: sdk.StackAlreadyExistsError,
        fullyQualifiedStackName,
    };
}

export { PulumiError };
