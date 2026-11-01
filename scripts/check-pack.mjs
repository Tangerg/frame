import { execFileSync } from "node:child_process";
import { access, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../", import.meta.url));
const directory = await mkdtemp(join(tmpdir(), "frame-package-consumer-"));
const npm = (args, cwd = directory) =>
  execFileSync(process.execPath, [process.env.npm_execpath, ...args], {
    cwd,
    encoding: "utf8",
    stdio: ["ignore", "pipe", "inherit"],
  });
const readJSON = async (path) => JSON.parse(await readFile(path, "utf8"));
const writeJSON = (path, value) => writeFile(path, JSON.stringify(value));

async function check(source, excluded = []) {
  for (const name of excluded) {
    try {
      await access(join(directory, "node_modules", name));
    } catch (error) {
      if (error.code === "ENOENT") continue;
      throw error;
    }
    throw new Error("minimal consumer installed " + name);
  }
  await writeFile(join(directory, "smoke.mjs"), source);
  await writeFile(join(directory, "smoke.mts"), source);
  execFileSync(process.execPath, ["smoke.mjs"], { cwd: directory });
  execFileSync(
    join(root, "node_modules/.bin/tsc"),
    [
      "--noEmit",
      "--strict",
      "--target",
      "ES2022",
      "--module",
      "NodeNext",
      "smoke.mts",
    ],
    { cwd: directory, stdio: "inherit" },
  );
}

try {
  const sdk = await readJSON(join(root, "sdk/package.json"));
  const [packed] = JSON.parse(
    npm(
      ["pack", "--json", "--ignore-scripts", "--pack-destination", directory],
      join(root, "sdk"),
    ),
  );
  const files = new Set(packed.files.map(({ path }) => "./" + path));
  for (const entry of Object.values(sdk.exports))
    for (const path of Object.values(entry))
      if (!files.has(path)) throw new Error("missing public entry " + path);

  const manifest = {
    name: "frame-external-consumer",
    version: "1.0.0",
    private: true,
    type: "module",
    dependencies: { [sdk.name]: "file:" + packed.filename },
  };
  // Use exact registry artifacts from the development lock; a consumer has no
  // workspace links, source aliases or access to the repository's node_modules.
  const lock = await readJSON(join(root, "package-lock.json"));
  const packages = {
    "": manifest,
    ["node_modules/" + sdk.name]: {
      ...lock.packages.sdk,
      resolved: manifest.dependencies[sdk.name],
      integrity: packed.integrity,
    },
  };
  function include(name) {
    const key = "node_modules/" + name;
    if (packages[key]) return;
    const entry = lock.packages[key];
    if (!entry || entry.link)
      throw new Error("missing external dependency " + name);
    packages[key] = entry;
    for (const child of Object.keys({
      ...entry.dependencies,
      ...entry.peerDependencies,
    }))
      include(child);
  }
  for (const name of Object.keys({
    ...sdk.dependencies,
    ...sdk.optionalDependencies,
    ...sdk.peerDependencies,
  }))
    include(name);
  manifest.devDependencies = {};
  for (const name of [
    "react",
    "react-dom",
    "@types/react",
    "@types/react-dom",
  ]) {
    include(name);
    manifest.devDependencies[name] =
      lock.packages["node_modules/" + name].version;
  }
  await writeJSON(join(directory, "package.json"), manifest);
  await writeJSON(join(directory, "package-lock.json"), {
    name: manifest.name,
    version: manifest.version,
    lockfileVersion: 3,
    requires: true,
    packages,
  });
  const install = ["ci", "--ignore-scripts", "--no-audit", "--no-fund"];
  npm([...install, "--omit=dev", "--omit=optional", "--omit=peer"]);
  const native = `import { defineUnit } from "@frame/sdk";
import { Lifetime } from "@frame/sdk/lifetime";
import { Runtime } from "@frame/sdk/browser";
import { PROTOCOL } from "@frame/sdk/protocol";
import { parseJSON } from "@frame/sdk/json";
if (![defineUnit, Lifetime, Runtime].every(v => typeof v === "function") || !PROTOCOL) throw new Error("broken public entry");
if (parseJSON("9007199254740993") !== 9007199254740993n) throw new Error("lost exact number");
`;
  await check(native, [
    ...Object.keys(sdk.optionalDependencies),
    ...Object.keys(sdk.peerDependencies),
  ]);
  npm([...install, "--include=dev", "--include=optional", "--include=peer"]);
  await check(
    native +
      `import { mount } from "@frame/sdk/react";
import { defineProcessUnit, serve } from "@frame/sdk/node";
if (![mount,defineProcessUnit,serve].every(v => typeof v === "function")) throw new Error("broken optional entry");`,
  );
  console.log(
    "Installed SDK tarball passed minimal browser and full Node/React consumer checks.",
  );
} finally {
  await rm(directory, { recursive: true, force: true });
}
