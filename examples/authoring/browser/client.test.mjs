import assert from "node:assert/strict";
import { test } from "node:test";
import { setImmediate } from "node:timers/promises";
import { PROTOCOL } from "@frame/sdk/protocol";
import { connect, sha256 } from "./dist/client.mjs";
import { applicationPaths } from "./src/contracts.generated.ts";

const authority = { id: "author-host", epoch: "1" };
const realm = { id: "author-window", epoch: "1" };
const credentials = {
  authority,
  realm,
  token: "author-credential",
  resumeToken: "author-resume-credential",
  maxMessageBytes: 1 << 20,
};

function json(value) {
  return new Response(JSON.stringify(value), { status: 200 });
}

function fixture(t, application, control) {
  const requests = [];
  const release = [];
  t.mock.method(globalThis, "fetch", async (url, options) => {
    const path = new URL(url).pathname;
    requests.push({ path, ...options });
    if (path === applicationPaths.control) {
      const { method, input } = JSON.parse(options.body);
      const response = await control?.(method, input, options.signal);
      if (response !== undefined) return response;
      switch (method) {
        case "poll":
          return json({ authority, realm, plans: [], fences: [] });
        case "admitPlan":
          return json({
            authority,
            realm,
            execution: input.execution,
            decision: "admitted",
          });
        case "ready":
          return json({ decision: "accepted" });
        case "prove":
          return json({
            closeId: input.closeId,
            proofId: input.proofId,
            recorded: true,
            authority,
          });
        default:
          throw new Error(`unexpected control request: ${method}`);
      }
    }
    const response = await application?.(path, options);
    if (response !== undefined) return response;
    if (path === applicationPaths.sealRealm)
      return new Response(null, { status: 204 });
    throw new Error(`unexpected application request: ${path}`);
  });
  const client = connect({ origin: "http://localhost:8080", credentials });
  t.after(async () => {
    for (const finish of release) await finish();
    await client.close();
  });
  return { client, requests, release };
}

test("close seals application admission, cancels requests, and joins their actual completion", async (t) => {
  const activation = Promise.withResolvers();
  const dispatched = Promise.withResolvers();
  const sealed = Promise.withResolvers();
  let reentrantClose;
  const { client, requests, release } = fixture(t, (path, options) => {
    if (path === applicationPaths.activate) {
      options.signal.addEventListener(
        "abort",
        () => {
          reentrantClose = client.close();
        },
        { once: true },
      );
      dispatched.resolve(options.signal);
      return activation.promise;
    }
    if (path === applicationPaths.sealRealm) sealed.resolve(options.signal);
  });
  release.push(() => activation.resolve(new Response(null, { status: 204 })));
  const activating = client.activate("converter");
  const signal = await dispatched.promise;
  let closed = false;
  const closing = client.close();
  closing.then(() => {
    closed = true;
  });
  assert.equal(reentrantClose, closing);
  assert.equal(client.close(), closing);
  assert.equal(signal.aborted, true);
  assert.equal((await sealed.promise).aborted, false);
  await setImmediate();
  assert.equal(closed, false);
  await assert.rejects(client.activate("converter"), { code: "closed" });
  await assert.rejects(client.activity(), { code: "closed" });
  assert.throws(() => client.createConversion({}), { code: "closed" });
  assert.equal(
    requests.filter(({ path }) => path === applicationPaths.activate).length,
    1,
  );
  assert.equal(
    requests.some(({ path }) => path === applicationPaths.activity),
    false,
  );

  activation.resolve(new Response(null, { status: 204 }));
  await activating;
  await closing;
  assert.equal(client.runtime.connection, "closed");
  for (const request of requests)
    assert.equal(request.headers.Authorization, `Bearer ${credentials.token}`);
});

test("application ownership includes the response body after headers arrive", async (t) => {
  const dispatched = Promise.withResolvers();
  let body;
  let finished = false;
  const finish = () => {
    if (finished) return;
    finished = true;
    body.enqueue(new TextEncoder().encode('1,"cancelled":0}'));
    body.close();
  };
  const { client, release } = fixture(t, (path, options) => {
    if (path !== applicationPaths.activity) return;
    const response = new Response(
      new ReadableStream({
        start(controller) {
          body = controller;
          body.enqueue(new TextEncoder().encode('{"started":'));
        },
      }),
    );
    dispatched.resolve(options.signal);
    return response;
  });
  release.push(finish);
  const activity = client.activity();
  const signal = await dispatched.promise;
  await setImmediate();
  let closed = false;
  const closing = client.close();
  closing.then(() => {
    closed = true;
  });
  assert.equal(signal.aborted, true);
  await setImmediate();
  assert.equal(closed, false);
  finish();
  assert.deepEqual(await activity, { started: 1, cancelled: 0 });
  await closing;
});

test("cancelling a dispatched activation reports an unknown outcome and still seals the Realm", async (t) => {
  const dispatched = Promise.withResolvers();
  const { client, requests } = fixture(t, (path, options) => {
    if (path !== applicationPaths.activate) return;
    dispatched.resolve(options.signal);
    return new Promise((_resolve, reject) => {
      options.signal.addEventListener(
        "abort",
        () => reject(options.signal.reason),
        {
          once: true,
        },
      );
    });
  });
  const activating = client.activate("converter");
  const signal = await dispatched.promise;
  const cancelled = assert.rejects(activating, (error) => {
    assert.equal(error.code, "outcome_unknown");
    assert.equal(error.cause, signal.reason);
    return true;
  });
  await client.close();
  await cancelled;
  assert.equal(
    requests.filter(({ path }) => path === applicationPaths.sealRealm).length,
    1,
  );
  assert.equal(client.runtime.connection, "closed");
});

for (const failure of ["lost acknowledgement", "invalid acknowledgement"]) {
  test(`${failure} keeps the original cleanup channel available for a close retry`, async (t) => {
    let attempts = 0;
    const { client, requests } = fixture(t, (path) => {
      if (path !== applicationPaths.sealRealm || ++attempts !== 1) return;
      if (failure === "lost acknowledgement")
        throw new Error("seal response was lost");
      return new Response("unexpected successful response", { status: 200 });
    });
    const closing = client.close();
    assert.equal(client.close(), closing);
    await assert.rejects(closing, { code: "outcome_unknown" });
    assert.equal(client.runtime.connection, "connected");
    assert.deepEqual(await client.control.poll(), {
      authority,
      realm,
      plans: [],
      fences: [],
    });
    await assert.rejects(client.activate("converter"), { code: "closed" });
    await client.close();
    assert.equal(attempts, 2);
    assert.equal(client.runtime.connection, "closed");
    for (const request of requests.filter(
      ({ path }) => path === applicationPaths.sealRealm,
    ))
      assert.equal(
        request.headers.Authorization,
        `Bearer ${credentials.token}`,
      );
  });
}

test("close keeps proof transport alive until the original execution is acknowledged", async (t) => {
  const module = new TextEncoder().encode(
    'export const units = [{id:"empty", imports:{}, exports:{}, setup(){return {}}}];',
  );
  const digest = await sha256(module);
  const proving = Promise.withResolvers();
  const acknowledgement = Promise.withResolvers();
  const { client, release } = fixture(
    t,
    (path) => {
      if (path === "/plugin.mjs") return new Response(module);
    },
    (method, input, signal) => {
      if (method !== "prove") return;
      proving.resolve({ proof: input, signal });
      return acknowledgement.promise;
    },
  );
  let proof;
  release.push(() => {
    if (proof)
      acknowledgement.resolve(
        json({
          closeId: proof.closeId,
          proofId: proof.proofId,
          recorded: true,
          authority,
        }),
      );
  });
  await client.runtime.prepare({
    protocol: PROTOCOL,
    approvalToken: "approved-empty-execution",
    authority,
    realm,
    execution: { id: "empty-execution", generation: "1" },
    scopeId: "empty-scope",
    scopeGeneration: "1",
    units: [
      {
        instance: {
          id: "empty-instance",
          generation: "1",
          source: {
            installationId: "author",
            release: digest,
            unitId: "empty",
          },
          configuration: digest,
        },
        configurationJson: "{}",
        profile: "browser-js",
        module: { path: "/plugin.mjs", digest },
        imports: [],
        exports: [],
      },
    ],
  });
  let closed = false;
  const closing = client.close();
  closing.then(() => {
    closed = true;
  });
  const observed = await proving.promise;
  proof = observed.proof;
  assert.equal(proof.execution.id, "empty-execution");
  assert.equal(proof.scopeKind, "execution");
  assert.equal(observed.signal.aborted, false);
  await setImmediate();
  assert.equal(closed, false);
  assert.equal(client.runtime.pendingProofs.length, 1);
  acknowledgement.resolve(
    json({
      closeId: proof.closeId,
      proofId: proof.proofId,
      recorded: true,
      authority,
    }),
  );
  await closing;
  assert.equal(client.runtime.pendingProofs.length, 0);
  assert.equal(observed.signal.aborted, true);
});
