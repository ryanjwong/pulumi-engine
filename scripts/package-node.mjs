#!/usr/bin/env node
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

// Package the Node binding the way esbuild does: one main package that holds
// the JavaScript and declares one optionalDependency per platform, and one
// package per platform that holds nothing but the shared library. npm
// installs only the platform package whose `os`/`cpu` match the host; the
// binding resolves it at load time (bindings/nodejs/src/native.ts).
//
//   node scripts/package-node.mjs --version 0.2.0 --libs dist --out dist/npm \
//       [--scope @ryanjwong] [--registry https://npm.pkg.github.com] [--pack]
//
// --libs is a directory holding libpulumi-<os>-<arch>.tar.gz tarballs
// (scripts/dist-lib.sh); one platform package is produced per tarball. The
// main package lists every platform the release knows (--platforms, default
// the four released ones) as optionalDependencies, whether or not a tarball
// for it was given, so a partial local build still produces a package.json
// that resolves on every platform once the release is published.
//
// The source package keeps the name @pulumi-engine/node (imports, tests and
// the workspace use it); the published name is <scope>/pulumi-engine-node
// because GitHub Packages requires the scope to be the repository owner.
// docs/releasing.md explains how consumers alias the two.

import { execFileSync } from "node:child_process";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const binding = path.join(root, "bindings", "nodejs");

function arg(name, fallback) {
    const i = process.argv.indexOf(`--${name}`);
    if (i < 0) {
        return fallback;
    }
    const v = process.argv[i + 1];
    if (v === undefined || v.startsWith("--")) {
        return true;
    }
    return v;
}

const version = arg("version");
if (typeof version !== "string" || !/^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/.test(version)) {
    console.error("usage: package-node.mjs --version X.Y.Z[-pre] --libs DIR --out DIR [--scope @owner] [--registry URL] [--pack]");
    process.exit(2);
}
const scope = arg("scope", "@ryanjwong");
const registry = arg("registry", "https://npm.pkg.github.com");
const libsDir = path.resolve(arg("libs", "dist"));
const outDir = path.resolve(arg("out", "dist/npm"));
const pack = arg("pack", false) === true;
const platforms = String(arg("platforms", "darwin-arm64,darwin-amd64,linux-amd64,linux-arm64")).split(",");

const mainName = `${scope}/pulumi-engine-node`;
const platformName = (key) => `${scope}/pulumi-engine-node-${key}`;
const npmCpu = { amd64: "x64", arm64: "arm64" };
const libExt = (osName) => (osName === "darwin" ? "dylib" : "so");

const source = JSON.parse(fs.readFileSync(path.join(binding, "package.json"), "utf8"));
if (!fs.existsSync(path.join(binding, "dist", "src", "index.js"))) {
    console.error(`${binding}/dist is missing; run 'pnpm build' (make node-build) first`);
    process.exit(2);
}

fs.rmSync(outDir, { recursive: true, force: true });
fs.mkdirSync(outDir, { recursive: true });

const common = {
    version,
    license: source.license,
    author: source.author,
    repository: { type: "git", url: "git+https://github.com/ryanjwong/pulumi-engine.git" },
    homepage: "https://github.com/ryanjwong/pulumi-engine#readme",
    publishConfig: { registry },
    engines: source.engines,
};

// Platform packages: one per tarball found.
const built = [];
for (const key of platforms) {
    const tarball = path.join(libsDir, `libpulumi-${key}.tar.gz`);
    if (!fs.existsSync(tarball)) {
        continue;
    }
    const [osName, arch] = key.split("-");
    const dir = path.join(outDir, `pulumi-engine-node-${key}`);
    fs.mkdirSync(dir, { recursive: true });
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "package-node-"));
    execFileSync("tar", ["-xzf", tarball, "-C", tmp]);
    const lib = `libpulumi.${libExt(osName)}`;
    fs.copyFileSync(path.join(tmp, lib), path.join(dir, lib));
    fs.rmSync(tmp, { recursive: true, force: true });
    fs.copyFileSync(path.join(root, "LICENSE"), path.join(dir, "LICENSE"));
    fs.writeFileSync(
        path.join(dir, "package.json"),
        JSON.stringify(
            {
                name: platformName(key),
                description: `libpulumi for ${key}: the shared library behind ${mainName}. Not meant to be installed directly.`,
                ...common,
                os: [osName],
                cpu: [npmCpu[arch] ?? arch],
                files: [lib, "LICENSE"],
            },
            null,
            2,
        ) + "\n",
    );
    fs.writeFileSync(
        path.join(dir, "README.md"),
        `# ${platformName(key)}\n\nThe \`libpulumi\` shared library for ${key}, installed as an optional dependency of \`${mainName}\`. Do not depend on it directly.\n`,
    );
    built.push({ key, name: platformName(key), dir });
}

// The main package.
const mainDir = path.join(outDir, "pulumi-engine-node");
fs.mkdirSync(path.join(mainDir, "dist"), { recursive: true });
fs.cpSync(path.join(binding, "dist", "src"), path.join(mainDir, "dist", "src"), { recursive: true });
fs.copyFileSync(path.join(binding, "README.md"), path.join(mainDir, "README.md"));
fs.copyFileSync(path.join(root, "LICENSE"), path.join(mainDir, "LICENSE"));
const optionalDependencies = {};
const platformPackages = {};
for (const key of platforms) {
    optionalDependencies[platformName(key)] = version;
    platformPackages[key] = platformName(key);
}
fs.writeFileSync(
    path.join(mainDir, "package.json"),
    JSON.stringify(
        {
            name: mainName,
            description: source.description,
            ...common,
            type: source.type,
            main: source.main,
            types: source.types,
            exports: source.exports,
            files: ["dist", "README.md", "LICENSE"],
            dependencies: source.dependencies,
            optionalDependencies,
            peerDependencies: source.peerDependencies,
            peerDependenciesMeta: source.peerDependenciesMeta,
            pulumiEngine: { sourceName: source.name, platformPackages },
        },
        null,
        2,
    ) + "\n",
);

// Publish order: platform packages first so the main package's optional
// dependencies resolve as soon as it appears.
const manifest = {
    version,
    registry,
    packages: [...built.map((b) => ({ name: b.name, dir: b.dir, platform: b.key })), { name: mainName, dir: mainDir }],
};
if (pack) {
    for (const p of manifest.packages) {
        const out = execFileSync("npm", ["pack", "--pack-destination", outDir, "--silent"], { cwd: p.dir, encoding: "utf8" });
        p.tarball = path.join(outDir, out.trim().split("\n").pop());
    }
}
fs.writeFileSync(path.join(outDir, "manifest.json"), JSON.stringify(manifest, null, 2) + "\n");
for (const p of manifest.packages) {
    console.log(`${p.name}@${version}  ${path.relative(root, p.tarball ?? p.dir)}`);
}
if (built.length === 0) {
    console.log(`(no libpulumi-<os>-<arch>.tar.gz found in ${libsDir}; only the main package was produced)`);
}
