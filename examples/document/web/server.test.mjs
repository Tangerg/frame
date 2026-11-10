import assert from "node:assert/strict";
import { createServer } from "node:http";
import test from "node:test";
import { FrameError } from "@frame/sdk";
import { DocumentServer } from "./src/document-server.ts";
import { taskView } from "./task-fixture.mjs";

async function application(t, respond) {
  const requests = [];
  const server = createServer(async (request, response) => {
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    const body = Buffer.concat(chunks).toString("utf8");
    requests.push({
      method: request.method,
      path: request.url,
      input: body === "" ? undefined : JSON.parse(body),
    });
    const reply = respond(requests.at(-1));
    response.writeHead(reply.status ?? 200, {
      "Content-Type": "application/json",
    });
    response.end(JSON.stringify(reply.body));
  });
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  t.after(
    () =>
      new Promise((resolve, reject) => {
        server.close((error) => (error ? reject(error) : resolve()));
        server.closeIdleConnections();
      }),
  );
  const origin = `http://127.0.0.1:${server.address().port}`;
  return {
    client: new DocumentServer(origin, () => "application-token"),
    requests,
  };
}

test("application operation binds its request and decoded result", async (t) => {
  const task = taskView("original-task", "queued");
  const { client, requests } = await application(t, () => ({ body: task }));
  assert.deepEqual(
    await client.request("queue", { id: task.id, path: "hello.md" }),
    task,
  );
  assert.deepEqual(requests, [
    {
      method: "POST",
      path: "/api/queue",
      input: { id: task.id, path: "hello.md" },
    },
  ]);
});

test("application refuses an invalid request before dispatch", async (t) => {
  const { client, requests } = await application(t, () => ({ body: null }));
  await assert.rejects(
    client.request("queue", { id: "missing-path" }),
    TypeError,
  );
  await assert.rejects(client.request("__proto__", {}), TypeError);
  assert.deepEqual(requests, []);
});

test("application query input preserves the original task identity", async (t) => {
  const id = "a/b?query#fragment";
  const { client, requests } = await application(t, () => ({
    body: taskView(id),
  }));
  assert.equal((await client.request("task", { id })).id, id);
  assert.equal(
    new URL(requests[0].path, "http://example.test").searchParams.get("id"),
    id,
  );
  assert.equal(requests[0].input, undefined);
});

test("a malformed successful command response leaves its outcome unknown", async (t) => {
  for (const body of [
    null,
    { id: "incomplete" },
    { ...taskView("original-task"), state: "invented" },
  ]) {
    await t.test(JSON.stringify(body), async (t) => {
      const { client } = await application(t, () => ({ body }));
      await assert.rejects(
        client.request("run", { id: "original-task" }),
        (error) => {
          assert.equal(error.code, "outcome_unknown");
          assert.ok(error.cause instanceof TypeError);
          return true;
        },
      );
    });
  }
});

test("a malformed query response is refused at the HTTP boundary", async (t) => {
  const { client } = await application(t, () => ({ body: { css: 42 } }));
  await assert.rejects(client.request("theme", undefined), (error) => {
    assert.equal(error.code, "unavailable");
    assert.ok(error.cause instanceof TypeError);
    return true;
  });
});

test("application failures retain their validated message", async (t) => {
  const { client } = await application(t, () => ({
    status: 409,
    body: { code: "conflict", error: "original task is unsettled" },
  }));
  await assert.rejects(
    client.request("run", { id: "original-task" }),
    /original task is unsettled/,
  );
});

test("application transport failures preserve their original cause and outcome classification", async (t) => {
  for (const scenario of [
    {
      name: "command fails before dispatch",
      operation: "run",
      input: { id: "original-task" },
      dispatched: false,
      code: "outcome_unknown",
    },
    {
      name: "command reply is lost after dispatch",
      operation: "run",
      input: { id: "original-task" },
      dispatched: true,
      code: "outcome_unknown",
    },
    {
      name: "query reply is lost after dispatch",
      operation: "theme",
      input: undefined,
      dispatched: true,
      code: "unavailable",
    },
  ]) {
    await t.test(scenario.name, async (t) => {
      const { client, requests } = await application(t, () => ({ body: null }));
      const cause = new Error(scenario.name);
      const originalFetch = globalThis.fetch;
      t.after(() => {
        globalThis.fetch = originalFetch;
      });
      globalThis.fetch = async (input, init) => {
        if (scenario.dispatched) {
          const response = await originalFetch(input, init);
          await response.arrayBuffer();
        }
        throw cause;
      };
      await assert.rejects(
        client.request(scenario.operation, scenario.input),
        (error) => {
          assert.ok(error instanceof FrameError);
          assert.equal(error.code, scenario.code);
          assert.equal(error.cause, cause);
          assert.ok(error.message.includes(scenario.operation));
          return true;
        },
      );
      assert.equal(requests.length, scenario.dispatched ? 1 : 0);
      if (scenario.dispatched)
        assert.deepEqual(requests[0].input, scenario.input);
    });
  }
});
