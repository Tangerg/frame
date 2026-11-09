import { expect, it } from "vitest";
import { observePromise } from "./promise-observation.js";

it("releases a cancelled observer while the original operation is still pending", async () => {
  const operation = Promise.withResolvers<number>();
  const remaining = observePromise(operation.promise);
  const cancelObservation = async () => {
    const controller = new AbortController();
    const signal = new WeakRef(controller.signal);
    const observation = observePromise(operation.promise, controller.signal);
    controller.abort("left");
    await expect(observation).rejects.toBe("left");
    return signal;
  };
  const signal = await cancelObservation();
  for (let index = 0; index < 25 && signal.deref(); index++) {
    await new Promise<void>((resolve) => setImmediate(resolve));
    global.gc!();
  }
  expect(signal.deref()).toBeUndefined();
  operation.resolve(42);
  expect(await remaining).toBe(42);
});

it("observes only cancellation for a departed waiter without losing the operation's later failure", async () => {
  const operation = Promise.withResolvers<void>();
  const signal = AbortSignal.abort("already left");
  await expect(observePromise(operation.promise, signal)).rejects.toBe(
    "already left",
  );
  const failure = new Error("original delivery failed");
  operation.reject(failure);
  await expect(observePromise(operation.promise)).rejects.toBe(failure);
});
