import { describe, expect, it } from "vitest";
import { Lifetime } from "./lifetime";
import { FrameError } from "./errors";
import { acquire, withResource, type AcquiredResource } from "./use";

describe("acquired resource", () => {
  it("owns a late acquisition and rejects new borrowing atomically", async () => {
    const owner = new Lifetime("owner");
    let resolve!: (resource: { value: number; close(): void }) => void;
    let closes = 0;
    const pending = acquire(
      owner,
      "late",
      () =>
        new Promise<{ value: number; close(): void }>((yes) => {
          resolve = yes;
        }),
    );
    const closed = owner.close();
    resolve({
      value: 42,
      close() {
        closes++;
      },
    });
    await expect(pending).rejects.toThrow("arrived after");
    expect((await closed).status).toBe("released");
    expect(closes).toBe(1);
  });

  it("joins current borrows before releasing and shares one exit", async () => {
    const owner = new Lifetime("owner");
    let finish!: () => void;
    const order: string[] = [];
    const acquired = await acquire(owner, "value", () => ({
      value: 7,
      close() {
        order.push("closed");
      },
    }));
    const borrowed = acquired.with(
      (value) =>
        new Promise<number>((resolve) => {
          finish = () => resolve(value);
        }),
    );
    const first = acquired.close();
    const second = acquired.close();
    expect(() => acquired.with(() => 0)).toThrow("no longer accepts");
    expect(order).toEqual([]);
    finish();
    expect(await borrowed).toBe(7);
    expect((await first).closeId).toBe((await second).closeId);
    expect(order).toEqual(["closed"]);
    await owner.close();
    expect(order).toEqual(["closed"]);
  });

  it("retains cleanup diagnostics when an acquisition arrives after close", async () => {
    const owner = new Lifetime("owner");
    let deliver!: (resource: AcquiredResource<number>) => void;
    const operation = acquire(
      owner,
      "late",
      () =>
        new Promise<AcquiredResource<number>>((resolve) => {
          deliver = resolve;
        }),
    );
    const closing = owner.close();
    deliver({
      value: 42,
      close: () => ({ status: "released", error: new Error("cleanup failed") }),
    });
    await expect(operation).rejects.toMatchObject({
      errors: [
        { code: "closed" },
        { code: "close_failed", cause: { status: "released" } },
      ],
    });
    expect((await closing).status).toBe("released");
  });

  it("borrows ordinary results and closes resource before returning", async () => {
    const owner = new Lifetime("owner");
    let closed = false;
    const result = await withResource(
      owner,
      "value",
      () => ({
        value: 8,
        close() {
          closed = true;
        },
      }),
      (value) => value * 2,
    );
    expect(result).toBe(16);
    expect(closed).toBe(true);
    await owner.close();
  });

  it("reports bounded cleanup diagnostics even after confirmed release", async () => {
    const owner = new Lifetime("owner");
    const operation = withResource(
      owner,
      "value",
      () => ({
        value: 42,
        close: () => ({
          status: "released" as const,
          error: new Error("private detail"),
        }),
      }),
      (value) => value,
    );
    await expect(operation).rejects.toBeInstanceOf(FrameError);
    await expect(operation).rejects.toMatchObject({
      code: "close_failed",
      cause: {
        status: "released",
        pending: [],
        failures: [{ phase: "cleanup", category: "exception" }],
        omittedFailures: 0,
      },
    });
    expect((await owner.close()).status).toBe("released");
  });

  it("preserves both work rejection and released cleanup diagnostics", async () => {
    const owner = new Lifetime("owner");
    const primary = new Error("work rejected");
    const operation = withResource(
      owner,
      "value",
      () => ({
        value: 42,
        close: () => ({
          status: "released" as const,
          error: new Error("cleanup rejected"),
        }),
      }),
      () => {
        throw primary;
      },
    );
    await expect(operation).rejects.toBeInstanceOf(AggregateError);
    await expect(operation).rejects.toMatchObject({
      errors: [
        primary,
        { code: "close_failed", cause: { status: "released" } },
      ],
    });
    expect((await owner.close()).status).toBe("released");
  });

  it("returns the original work rejection when close adds no failure", async () => {
    const owner = new Lifetime("owner");
    const primary = new Error("work rejected");
    await expect(
      withResource(
        owner,
        "value",
        () => ({ value: 1, close() {} }),
        () => {
          throw primary;
        },
      ),
    ).rejects.toBe(primary);
    expect((await owner.close()).status).toBe("released");
  });

  it.each([false, true])(
    "retains unconfirmed evidence with work rejection=%s",
    async (failed) => {
      const owner = new Lifetime("owner");
      let confirm!: () => void;
      const primary = new Error("work rejected");
      const operation = withResource(
        owner,
        "value",
        (_signal, cleanup) => {
          confirm = () => cleanup.confirmReleased("termination observed");
          return {
            value: 1,
            close: () => ({ status: "unconfirmed" as const }),
          };
        },
        () => {
          if (failed) throw primary;
          return 42;
        },
      );
      try {
        const close = {
          code: "close_unconfirmed",
          cause: { status: "unconfirmed", pending: ["value"] },
        };
        await expect(operation).rejects.toMatchObject(
          failed ? { errors: [primary, close] } : close,
        );
      } finally {
        confirm();
        expect((await owner.wait()).status).toBe("released");
      }
    },
  );
});
