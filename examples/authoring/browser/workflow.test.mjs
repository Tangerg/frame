import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import test from "node:test";

const directory = fileURLToPath(new URL(".", import.meta.url));

// This author exercise executes native JavaScript in Node, the real Go Host,
// SQLite and HTTP. The separate document workflow tests an actual browser engine.
test(
  "public TS author tasks use the real Host and original resource owners",
  { timeout: 30000 },
  async (t) => {
    const binary = process.env.FRAME_AUTHOR_HOST_BINARY;
    assert.ok(binary, "build authorhost and set FRAME_AUTHOR_HOST_BINARY");
    const state = await mkdtemp(join(tmpdir(), "frame-public-author-"));
    const server = spawn(
      binary,
      [
        "-state",
        state,
        "-package",
        join(directory, "dist/units.json"),
        "-module",
        join(directory, "dist/plugin.mjs"),
      ],
      { stdio: ["ignore", "pipe", "pipe"] },
    );
    const exited = once(server, "exit");
    let diagnostics = "";
    let output = "";
    server.stderr.setEncoding("utf8").on("data", (chunk) => {
      diagnostics += chunk;
    });
    server.stdout.setEncoding("utf8").on("data", (chunk) => {
      output += chunk;
    });
    let client;
    let completed = false;
    try {
      await until(
        () => output.includes("\n"),
        () => diagnostics,
      );
      const bootstrap = JSON.parse(output.slice(0, output.indexOf("\n")));
      const { connect, sha256, validators } = await import(
        pathToFileURL(join(directory, "dist/client.mjs")).href
      );
      client = connect(bootstrap);
      await client.activate("converter");
      const reference = {
        path: "hello.md",
        revision: await sha256(
          new TextEncoder().encode("# Public author\nOriginal source.\n"),
        ),
      };

      await t.test(
        "failed selection closes its original snapshot through the real Host",
        async () => {
          const snapshot = client.control.snapshot.bind(client.control);
          const close = client.control.closeSnapshot.bind(client.control);
          const selected = [];
          const closed = [];
          client.control.snapshot = async (request) => {
            const value = await snapshot(request);
            selected.push(value.id);
            return value;
          };
          client.control.closeSnapshot = async (id) => {
            await close(id);
            closed.push(id);
          };
          try {
            for (let attempt = 0; attempt < 4; attempt++) {
              await assert.rejects(
                client.start(reference),
                /requires both Points/,
              );
            }
            assert.deepEqual(closed, selected);
            assert.equal(closed.length, 4);
          } finally {
            client.control.snapshot = snapshot;
            client.control.closeSnapshot = close;
          }
        },
      );
      await client.activate("validator");

      await t.test(
        "FileReader import and a single multi-Point Use return a resource",
        async () => {
          const task = await client.start(reference);
          const rendered = await task.result;
          let url;
          await rendered.with(async (resource) => {
            assert.equal(resource.document.title, "Public author");
            assert.deepEqual(resource.document.source, reference);
            assert.deepEqual(resource.diagnostics, []);
            url = resource.url;
            assert.match(
              await (await fetch(url)).text(),
              /<h1>Public author<\/h1>/,
            );
          });
          assert.equal((await rendered.close()).status, "released");
          await assert.rejects(fetch(url));
          await assert.rejects(async () => rendered.with(() => {}), {
            code: "closed",
          });
          await until(
            () => client.runtime.pendingProofs.length === 0,
            () => diagnostics,
          );
          const untitled = await client.start({
            path: "untitled.md",
            revision: await sha256(
              new TextEncoder().encode("A document without a title.\n"),
            ),
          });
          const validation = await untitled.result;
          await validation.with((resource) => {
            assert.equal(resource.document.title, "");
            assert.deepEqual(resource.diagnostics, [
              { rule: "title", message: "A document title is required." },
            ]);
          });
          assert.equal((await validation.close()).status, "released");
          await until(
            () => client.runtime.pendingProofs.length === 0,
            () => diagnostics,
          );
          assert.deepEqual(client.errors, []);
        },
      );

      await t.test(
        "closing the original Use aborts and joins the active FileReader",
        async () => {
          const task = await client.start({ ...reference, path: "wait.md" });
          let settled = false;
          void task.result.then(
            () => {
              settled = true;
            },
            () => {
              settled = true;
            },
          );
          const failed = assert.rejects(task.result);
          await until(
            async () => (await client.activity()).started === 1,
            () => diagnostics,
          );
          assert.equal(
            settled,
            false,
            "the source must still be pending when cancellation enters its original Use",
          );
          assert.equal((await task.close()).status, "released");
          await failed;
          assert.equal(
            settled,
            true,
            "Use release must join the cancelled converter",
          );
          await until(
            async () => (await client.activity()).cancelled === 1,
            () => diagnostics,
          );
          await until(
            () => client.runtime.pendingProofs.length === 0,
            () => diagnostics,
          );
        },
      );

      await t.test(
        "partial setup releases the actual staged resource without publication",
        async () => {
          await assert.rejects(client.activate("partial"));
          const failure = client.errors.find(
            (error) => error?.name === "PartialSetupFailure",
          );
          assert.ok(failure, client.errors.map(String).join("\n"));
          assert.equal(typeof failure.releasedURL, "string");
          await assert.rejects(fetch(failure.releasedURL));
          const snapshot = await client.control.snapshot({
            operationId: crypto.randomUUID(),
            points: [validators.ref.name],
          });
          assert.equal(
            snapshot.points[0].candidates.some(
              (candidate) => candidate.source.unitId === "partial",
            ),
            false,
          );
          await client.control.closeSnapshot(snapshot.id);
          await until(
            () => client.runtime.pendingProofs.length === 0,
            () => diagnostics,
          );
          assert.deepEqual(
            client.errors.map((error) => error.name),
            ["PartialSetupFailure"],
            client.errors
              .map((error) => `${error.code ?? error.name}: ${error.message}`)
              .join("\n"),
          );
        },
      );
      await client.close();
      client = undefined;
      completed = true;
    } finally {
      if (client)
        await client.close().catch((error) => {
          diagnostics += String(error);
        });
      if (server.exitCode === null)
        server.kill(completed ? "SIGINT" : "SIGKILL");
      const [code] = await exited;
      await rm(state, { recursive: true, force: true });
      if (completed) assert.equal(code, 0, diagnostics);
    }
  },
);

async function until(predicate, diagnostic) {
  const deadline = Date.now() + 10000;
  while (!(await predicate())) {
    if (Date.now() >= deadline)
      throw new Error("author workflow did not finish: " + diagnostic());
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
}
