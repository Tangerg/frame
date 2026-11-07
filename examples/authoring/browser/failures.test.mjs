import assert from "node:assert/strict";
import { test } from "node:test";
import { setImmediate } from "node:timers/promises";
import { ConnectionFailures } from "./src/failures.ts";

test("long control outages retain bounded diagnostics without thrown object graphs", () => {
  const failures = new ConnectionFailures();
  const cause = new Uint8Array(1 << 20);
  const message = "connection lost".repeat(10000);
  for (let attempt = 0; attempt < 1000; attempt++) {
    const error = new Error(message, { cause });
    failures.record(error);
  }
  assert.ok(failures.records.length > 0);
  assert.ok(failures.records.length < 1000);
  assert.equal(failures.records.length + failures.omitted, 1000);
  for (const failure of failures.records) {
    assert.deepEqual(Object.keys(failure), ["name", "message"]);
    assert.ok(failure.message.length < 1000);
    assert.ok(Object.isFrozen(failure));
  }
});

test("hostile thrown values cannot prevent later diagnostics", () => {
  const failures = new ConnectionFailures();
  const error = new Error("unreadable");
  Object.defineProperty(error, "message", {
    get() {
      throw new Error("message getter failed");
    },
  });
  failures.record(error);
  failures.record(
    new Proxy(
      {},
      {
        getPrototypeOf() {
          throw error;
        },
      },
    ),
  );
  failures.record(new Error("recovered"));
  assert.equal(failures.records.at(-1).message, "recovered");
});

test("throwing observers cannot stop later connection diagnostics", () => {
  const failures = new ConnectionFailures();
  const error = new Error("connection lost");
  const observerFailure = new Error("observer failed", { cause: error });
  let observed;
  assert.doesNotThrow(() =>
    failures.report(error, (original) => {
      observed = original;
      throw observerFailure;
    }),
  );
  assert.equal(observed, error);
  failures.report(new Error("later failure"));
  assert.deepEqual(failures.records, [
    { name: "Error", message: "connection lost" },
    { name: "Error", message: "observer failed" },
    { name: "Error", message: "later failure" },
  ]);
});

test("rejected async observers cannot stop later connection diagnostics", async () => {
  const failures = new ConnectionFailures();
  const error = new Error("connection lost");
  let observed;
  failures.report(error, async (original) => {
    observed = original;
    throw new Error("observer rejected", { cause: original });
  });
  assert.equal(observed, error);
  failures.report(new Error("later failure"));
  await setImmediate();
  assert.deepEqual(failures.records, [
    { name: "Error", message: "connection lost" },
    { name: "Error", message: "later failure" },
    { name: "Error", message: "observer rejected" },
  ]);
  failures.report(new Error("recovered"));
  assert.equal(failures.records.at(-1).message, "recovered");
});

test("hostile observer thenables cannot replace or repeat their settled failure", async () => {
  const failures = new ConnectionFailures();
  failures.report(
    new Error("first connection failure"),
    () =>
      new Proxy(Promise.resolve(), {
        get() {
          throw new Error("then getter failed");
        },
      }),
  );
  failures.report(
    new Error("second connection failure"),
    () =>
      new Proxy(Promise.resolve(), {
        get() {
          return (_resolve, reject) => {
            reject(new Error("thenable rejected"));
            reject(new Error("second rejection"));
            throw new Error("throw after rejection");
          };
        },
      }),
  );
  await setImmediate();
  assert.deepEqual(failures.records, [
    { name: "Error", message: "first connection failure" },
    { name: "Error", message: "second connection failure" },
    { name: "Error", message: "then getter failed" },
    { name: "Error", message: "thenable rejected" },
  ]);
  failures.report(new Error("recovered"));
  assert.equal(failures.records.at(-1).message, "recovered");
});
