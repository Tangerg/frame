import assert from "node:assert/strict";
import { test } from "node:test";
import { FrameError } from "@frame/sdk";
import { PreviewSnapshots } from "./src/preview-capture.ts";

test("snapshot retries preserve the original operation after an uncertain dispatch and a later refusal", async () => {
  const captures = new PreviewSnapshots("documents.preview");
  const requests = [];
  const replies = [
    new FrameError("outcome_unknown", "capture acknowledgement lost"),
    new FrameError("permission_denied", "credential expired"),
  ];
  const control = {
    async snapshot(request) {
      requests.push(request);
      const refusal = replies.shift();
      if (refusal) throw refusal;
      return { id: request.operationId, points: [] };
    },
  };
  await assert.rejects(captures.capture(control), { code: "outcome_unknown" });
  await assert.rejects(captures.capture(control), {
    code: "permission_denied",
  });
  const snapshot = await captures.capture(control);
  assert.equal(requests.length, 3);
  for (const request of requests) assert.strictEqual(request, requests[0]);
  assert.equal(snapshot.id, requests[0].operationId);
  assert.deepEqual(requests[0].points, ["documents.preview"]);

  await captures.capture(control);
  assert.notEqual(requests[3].operationId, snapshot.id);
});

test("concurrent observers share one pending capture", async () => {
  const captures = new PreviewSnapshots("documents.preview");
  let complete;
  let dispatches = 0;
  const control = {
    snapshot(request) {
      dispatches++;
      return new Promise((resolve) => {
        complete = () => resolve({ id: request.operationId, points: [] });
      });
    },
  };
  const first = captures.capture(control);
  const second = captures.capture(control);
  assert.strictEqual(second, first);
  await Promise.resolve();
  assert.equal(dispatches, 1);
  complete();
  assert.strictEqual(await first, await second);
});
