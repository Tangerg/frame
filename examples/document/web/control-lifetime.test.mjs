import assert from "node:assert/strict";
import test from "node:test";
import { Lifetime } from "@frame/sdk";
import { parseJSON } from "@frame/sdk/json";
import { DocumentServer } from "./src/document-server.ts";
import { PreviewSession } from "./src/preview-session.ts";

const authority = { id: "host", epoch: "1" };
const realm = { id: "realm", epoch: "1" };
const contract = {
  name: "preview",
  version: "1",
  digest: "a".repeat(64),
  kind: "point",
  mode: "native",
};

async function sessionFixture(t) {
  const originalFetch = globalThis.fetch;
  let resumed = 0;
  let refuseRevalidation = false;
  const controlRequests = [];
  const credentials = () => ({
    authority,
    realm,
    token: "credential:" + resumed,
    resumeToken: "original-resume",
    maxMessageBytes: 65536,
  });
  globalThis.fetch = async (input, init) => {
    const path = new URL(input, "http://localhost").pathname;
    if (path === "/api/realm/resume") {
      resumed++;
      return Response.json(credentials());
    }
    if (path === "/api/realm/seal") return Response.json(null);
    assert.equal(path, "/frame/control");
    const { method } = JSON.parse(init.body);
    controlRequests.push({ method, signal: init.signal });
    if (method === "poll")
      return Response.json({ authority, realm, plans: [], fences: [] });
    assert.equal(method, "reconnect");
    if (refuseRevalidation) {
      refuseRevalidation = false;
      throw new Error("revalidation transport interrupted");
    }
    return Response.json({
      authority,
      realm,
      mode: "revalidated",
      executions: [],
    });
  };
  const actions = new Lifetime("preview application actions");
  const server = new DocumentServer("http://localhost", () => "application");
  const session = new PreviewSession(
    server,
    parseJSON(JSON.stringify(credentials())),
    contract,
    actions,
    (error) => {
      throw error;
    },
  );
  t.after(async () => {
    try {
      await session.close();
      await actions.close();
    } finally {
      globalThis.fetch = originalFetch;
    }
  });
  return {
    session,
    controlRequests,
    get resumed() {
      return resumed;
    },
    refuseNextRevalidation() {
      refuseRevalidation = true;
    },
  };
}

test("releases each authenticated channel across more than 1024 cleanup replacements", async (t) => {
  const f = await sessionFixture(t);
  for (let index = 0; index < 1030; index++) {
    await f.session.close();
    assert.ok(f.controlRequests.every((request) => request.signal.aborted));
  }
  assert.equal(f.resumed, 1029);
});

test("releases an unsuccessful replacement without losing the original cleanup owner", async (t) => {
  const f = await sessionFixture(t);
  await f.session.close();
  f.refuseNextRevalidation();
  await assert.rejects(f.session.close());
  assert.ok(f.controlRequests.every((request) => request.signal.aborted));
  await f.session.close();
  assert.equal(f.resumed, 2);
  assert.ok(f.controlRequests.every((request) => request.signal.aborted));
});
