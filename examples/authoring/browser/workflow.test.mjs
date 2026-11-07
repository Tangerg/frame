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
      let releasedURL;
      client = connect(bootstrap, (error) => {
        if (error?.name === "PartialSetupFailure")
          releasedURL = error.releasedURL;
      });
      await client.activate("converter");
      const reference = {
        path: "hello.md",
        revision: await sha256(
          new TextEncoder().encode("# Public author\nOriginal source.\n"),
        ),
      };

      await t.test(
        "a first Snapshot refusal settles its task without a close request",
        async () => {
          const snapshot = client.control.snapshot.bind(client.control);
          const closeSnapshot = client.control.closeSnapshot.bind(
            client.control,
          );
          const closed = [];
          client.control.snapshot = (request) =>
            snapshot({ ...request, points: ["author.missing-point"] });
          client.control.closeSnapshot = async (id) => {
            closed.push(id);
            await closeSnapshot(id);
          };
          const task = client.createConversion(reference);
          try {
            await assert.rejects(task.run(), { code: "contract_mismatch" });
            await task.close();
            assert.deepEqual(closed, []);
            await assert.rejects(task.run(), { code: "closed" });
          } finally {
            client.control.snapshot = snapshot;
            client.control.closeSnapshot = closeSnapshot;
          }
        },
      );

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
                client.createConversion(reference).run(),
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
        "one task recovers a lost acquisition acknowledgement",
        async () => {
          const acquire = client.control.acquire.bind(client.control);
          const snapshot = client.control.snapshot.bind(client.control);
          const requests = [];
          const captures = [];
          client.control.snapshot = async (request) => {
            captures.push(request);
            return snapshot(request);
          };
          client.control.acquire = async (request) => {
            requests.push(request);
            const grant = await acquire(request);
            if (requests.length === 1)
              throw new Error("lost acquisition reply");
            return grant;
          };
          const task = client.createConversion(reference);
          try {
            assert.equal(requests.length, 0);
            assert.equal(captures.length, 0);
            await assert.rejects(task.run(), /lost acquisition reply/);
            assert.equal(
              client.runtime.hasPendingAcquisition(task.operationId),
              true,
            );
            const [rendered, concurrent] = await Promise.all([
              task.run(),
              task.run(),
            ]);
            assert.equal(rendered, concurrent);
            await rendered.with((resource) =>
              assert.equal(resource.document.title, "Public author"),
            );
            assert.equal(captures.length, 1);
            assert.equal(requests.length, 2);
            assert.deepEqual(requests[0], requests[1]);
            assert.equal(requests[0].operationId, task.operationId);
            await task.close();
            assert.equal(
              client.runtime.hasPendingAcquisition(task.operationId),
              false,
            );
            await assert.rejects(task.run(), { code: "closed" });
          } finally {
            client.control.acquire = acquire;
            client.control.snapshot = snapshot;
            await task.close();
          }
        },
      );

      await t.test(
        "closing an uncertain task recovers its Use without running conversion",
        async () => {
          const acquire = client.control.acquire.bind(client.control);
          const requests = [];
          const before = await client.activity();
          client.control.acquire = async (request) => {
            requests.push(request);
            const grant = await acquire(request);
            if (requests.length <= 2) throw new Error("lost acquisition reply");
            return grant;
          };
          const task = client.createConversion({
            ...reference,
            path: "wait.md",
          });
          try {
            await assert.rejects(task.run(), /lost acquisition reply/);
            await assert.rejects(task.close(), /lost acquisition reply/);
            assert.equal(
              client.runtime.hasPendingAcquisition(task.operationId),
              true,
            );
            await task.close();
            assert.equal(requests.length, 3);
            assert.deepEqual(requests[0], requests[1]);
            assert.deepEqual(requests[1], requests[2]);
            assert.equal(
              client.runtime.hasPendingAcquisition(task.operationId),
              false,
            );
            assert.deepEqual(await client.activity(), before);
          } finally {
            client.control.acquire = acquire;
            await task.close();
          }
        },
      );

      await t.test(
        "one task retries the original Snapshot after its reply is lost",
        async () => {
          const snapshot = client.control.snapshot.bind(client.control);
          const captures = [];
          client.control.snapshot = async (request) => {
            captures.push(request);
            const captured = await snapshot(request);
            if (captures.length === 1) throw new Error("lost snapshot reply");
            return captured;
          };
          const task = client.createConversion(reference);
          try {
            await assert.rejects(task.run(), /lost snapshot reply/);
            const rendered = await task.run();
            await rendered.with((resource) =>
              assert.equal(resource.document.title, "Public author"),
            );
            assert.equal(captures.length, 2);
            assert.deepEqual(captures[0], captures[1]);
          } finally {
            client.control.snapshot = snapshot;
            await task.close();
          }
        },
      );

      await t.test(
        "a refused retry preserves an earlier uncertain Snapshot for close",
        async () => {
          const snapshot = client.control.snapshot.bind(client.control);
          const closeSnapshot = client.control.closeSnapshot.bind(
            client.control,
          );
          const refusal = await snapshot({
            operationId: crypto.randomUUID(),
            points: ["author.missing-point"],
          }).catch((error) => error);
          assert.equal(refusal.code, "contract_mismatch");
          const captures = [];
          const closed = [];
          client.control.snapshot = async (request) => {
            captures.push(request);
            if (captures.length > 1) throw refusal;
            await snapshot(request);
            throw new Error("lost snapshot reply");
          };
          client.control.closeSnapshot = async (id) => {
            await closeSnapshot(id);
            closed.push(id);
          };
          const task = client.createConversion(reference);
          try {
            await assert.rejects(task.run(), /lost snapshot reply/);
            await assert.rejects(task.run(), { code: "contract_mismatch" });
            assert.equal(captures.length, 2);
            assert.deepEqual(captures[0], captures[1]);
            await task.close();
            assert.deepEqual(closed, [captures[0].operationId]);
          } finally {
            client.control.snapshot = snapshot;
            client.control.closeSnapshot = closeSnapshot;
            await task.close();
          }
        },
      );

      await t.test(
        "FileReader import and a single multi-Point Use return a resource",
        async () => {
          const task = client.createConversion(reference);
          const rendered = await task.run();
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
          const untitled = client.createConversion({
            path: "untitled.md",
            revision: await sha256(
              new TextEncoder().encode("A document without a title.\n"),
            ),
          });
          const validation = await untitled.run();
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
          assert.deepEqual(client.failures, []);
        },
      );

      await t.test(
        "closing the original Use aborts and joins the active FileReader",
        async () => {
          const task = client.createConversion({
            ...reference,
            path: "wait.md",
          });
          const result = task.run();
          let settled = false;
          void result.then(
            () => {
              settled = true;
            },
            () => {
              settled = true;
            },
          );
          const failed = assert.rejects(result);
          await until(
            async () => (await client.activity()).started === 1,
            () => diagnostics,
          );
          assert.equal(
            settled,
            false,
            "the source must still be pending when cancellation enters its original Use",
          );
          await task.close();
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
          const failure = client.failures.find(
            (error) => error?.name === "PartialSetupFailure",
          );
          assert.ok(failure, client.failures.map(String).join("\n"));
          assert.equal(typeof releasedURL, "string");
          await assert.rejects(fetch(releasedURL));
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
            client.failures.map((error) => error.name),
            ["PartialSetupFailure"],
            client.failures
              .map((error) => `${error.code ?? error.name}: ${error.message}`)
              .join("\n"),
          );
        },
      );
      await t.test(
        "application shutdown closes a task whose Snapshot reply was lost",
        async () => {
          const snapshot = client.control.snapshot.bind(client.control);
          const closeSnapshot = client.control.closeSnapshot.bind(
            client.control,
          );
          let captured;
          const closed = [];
          client.control.snapshot = async (request) => {
            captured = request.operationId;
            await snapshot(request);
            throw new Error("lost snapshot reply");
          };
          client.control.closeSnapshot = async (id) => {
            await closeSnapshot(id);
            closed.push(id);
          };
          await assert.rejects(
            client.createConversion(reference).run(),
            /lost snapshot reply/,
          );
          await client.close();
          assert.deepEqual(closed, [captured]);
          assert.equal(client.runtime.pendingProofs.length, 0);
        },
      );
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
