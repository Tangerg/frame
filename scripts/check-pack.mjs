import { execFileSync } from "node:child_process";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../", import.meta.url));
const directory = await mkdtemp(join(tmpdir(), "frame-package-consumer-"));
const npm = (args, cwd) =>
  execFileSync(process.execPath, [process.env.npm_execpath, ...args], {
    cwd,
    encoding: "utf8",
    env: process.env,
    stdio: ["ignore", "pipe", "inherit"],
  });

try {
  const lock = JSON.parse(
    await readFile(join(root, "package-lock.json"), "utf8"),
  );
  const dependencies = {};
  const packages = {};
  for (const name of ["sdk", "node", "browser", "react"]) {
    const source = "packages/" + name;
    const manifest = JSON.parse(
      await readFile(join(root, source, "package.json"), "utf8"),
    );
    const [packed] = JSON.parse(
      npm(
        ["pack", "--json", "--ignore-scripts", "--pack-destination", directory],
        join(root, source),
      ),
    );
    if (!packed.files.some((file) => file.path === "dist/index.d.ts"))
      throw new Error("missing public type declarations in " + manifest.name);
    dependencies[manifest.name] = "file:" + packed.filename;
    packages["node_modules/" + manifest.name] = {
      version: manifest.version,
      resolved: dependencies[manifest.name],
      integrity: packed.integrity,
      dependencies: manifest.dependencies,
      peerDependencies: manifest.peerDependencies,
    };
  }
  for (const name of [
    "@types/react",
    "@types/react-dom",
    "@types/node",
    "react",
    "react-dom",
  ])
    dependencies[name] = lock.packages["node_modules/" + name].version;

  // A real immutable tarball replaces each workspace link. Resolve the exact
  // production closure from the public lock so the check also works offline.
  function include(name) {
    const key = "node_modules/" + name;
    if (packages[key]) return;
    const entry = lock.packages[key];
    if (!entry || entry.link)
      throw new Error("missing independent dependency " + name);
    const { dev: _dev, devOptional: _devOptional, ...production } = entry;
    packages[key] = production;
    for (const child of Object.keys({
      ...entry.dependencies,
      ...entry.peerDependencies,
    }))
      include(child);
  }
  for (const entry of Object.values(packages))
    for (const child of Object.keys({
      ...entry.dependencies,
      ...entry.peerDependencies,
    }))
      include(child);
  for (const name of Object.keys(dependencies)) include(name);
  const manifest = {
    name: "frame-external-consumer",
    version: "1.0.0",
    private: true,
    type: "module",
    dependencies,
  };
  await writeFile(join(directory, "package.json"), JSON.stringify(manifest));
  await writeFile(
    join(directory, "package-lock.json"),
    JSON.stringify({
      name: manifest.name,
      version: manifest.version,
      lockfileVersion: 3,
      requires: true,
      packages: { "": manifest, ...packages },
    }),
  );
  npm(["ci", "--ignore-scripts", "--no-audit", "--no-fund"], directory);
  const source = `import { defineUnit } from "@frame/sdk";
import { parseJSON } from "@frame/sdk/json";
import { Runtime } from "@frame/browser";
import { mount } from "@frame/react";
import { defineProcessUnit, serve } from "@frame/node";
if (![defineUnit,Runtime,mount,defineProcessUnit,serve].every(v => typeof v === "function")) throw new Error("broken public entry");
if (parseJSON("9007199254740993") !== 9007199254740993n) throw new Error("lost exact number");
`;
  await writeFile(join(directory, "smoke.mjs"), source);
  await writeFile(join(directory, "smoke.mts"), source);
  execFileSync(process.execPath, ["smoke.mjs"], {
    cwd: directory,
    stdio: "inherit",
  });
  execFileSync(
    join(root, "node_modules/.bin/tsc"),
    [
      "--noEmit",
      "--strict",
      "--target",
      "ES2022",
      "--module",
      "NodeNext",
      "--moduleResolution",
      "NodeNext",
      "smoke.mts",
    ],
    { cwd: directory, stdio: "inherit" },
  );
  console.log(
    "Four independently installed ESM tarballs imported and type-checked.",
  );
} finally {
  await rm(directory, { recursive: true, force: true });
}
