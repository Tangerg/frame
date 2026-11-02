#!/usr/bin/env node
import { execFileSync } from "node:child_process";
import { mkdtemp, readdir, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

if (process.argv.length !== 3)
  throw new Error("usage: check-flame-interop.mjs path/to/flame-checkout");
const flame = resolve(process.argv[2]);
const frame = fileURLToPath(new URL("../", import.meta.url));
const fixtures = join(frame, "process/mcp/testdata");
const work = await mkdtemp(join(tmpdir(), "frame-flame-"));

// A managed Node backend and a predeclared MCP server can both own contributions.
// The same Flame build plan that packages them defines this qualification corpus.
async function contractCorpus() {
  const { readBuildPlan } = await import(
    pathToFileURL(join(flame, "plugins/scripts/build.mjs")).href
  );
  const corpus = [];
  for (const plugin of await readdir(join(flame, "plugins"), {
    withFileTypes: true,
  })) {
    if (!plugin.isDirectory()) continue;
    const source = join(flame, "plugins", plugin.name);
    try {
      await stat(join(source, "package"));
    } catch (e) {
      if (e.code === "ENOENT") continue;
      throw e;
    }
    const plan = await readBuildPlan(source);
    const servers = new Set([
      ...Object.keys(plan.mcp?.mcpServers ?? {}),
      ...plan.managed.map((backend) => backend.id),
    ]);
    const contracts = [];
    for (const contribution of plan.declaration.contributions ?? []) {
      if (!contribution.server) continue;
      if (!servers.has(contribution.server))
        throw new Error(
          plugin.name + ": undeclared server " + contribution.server,
        );
      contracts.push(contribution.contract);
    }
    if (contracts.length) corpus.push({ plugin: plugin.name, contracts });
  }
  return corpus;
}

try {
  // Flame owns the source pins for unpublished workspace modules.
  const { moduleArtifacts, proxyEnvironment } = await import(
    pathToFileURL(join(flame, "tooling/workspace.mjs")).href
  );
  const artifacts = await moduleArtifacts({
    repository: flame,
    output: join(work, "proxy"),
  });
  const env = {
    ...proxyEnvironment(artifacts.proxy),
    GOWORK: join(flame, "go.work"),
  };
  const inventory = join(work, "contract-corpus.json");
  await writeFile(inventory, JSON.stringify(await contractCorpus()));
  const binary = (name) => join(work, name);
  for (const [name, source] of [
    ["hooks", "./plugins/hooks/backend"],
    ["trajectory", "./plugins/trajectory/backend"],
    ["stream", join(fixtures, "qimu-stream/main.go")],
  ])
    execFileSync("go", ["build", "-o", binary(name), source], {
      cwd: flame,
      env,
      stdio: "inherit",
    });

  const sample = join(work, "document.json");
  await writeFile(
    sample,
    execFileSync("go", ["run", join(fixtures, "trajectory/main.go")], {
      cwd: flame,
      env,
    }),
  );
  execFileSync(
    "go",
    [
      "test",
      "-race",
      "-tags",
      "flameinterop",
      "-count=1",
      "-v",
      "./process/mcp",
    ],
    {
      cwd: frame,
      stdio: "inherit",
      env: {
        ...process.env,
        GOWORK: "off",
        FRAME_FLAME_ROOT: flame,
        FRAME_FLAME_CORPUS: inventory,
        FRAME_FLAME_HOOKS: binary("hooks"),
        FRAME_FLAME_HOOKS_CONTRACT: join(
          flame,
          "plugins/hooks/package/io.github.tangerg.flame/contracts/capability.generated.json",
        ),
        FRAME_QIMU_STREAM: binary("stream"),
        FRAME_QIMU_VECTORS: join(
          flame,
          "qimu/node/src/protocol.generated.json",
        ),
        FRAME_FLAME_TRAJECTORY: binary("trajectory"),
        FRAME_TRAJECTORY_INPUT: sample,
      },
    },
  );
} finally {
  await rm(work, { recursive: true, force: true });
}
