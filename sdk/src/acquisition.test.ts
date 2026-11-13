import { describe, expect, it } from "vitest";
import { Lifetime, type Cleanup } from "./lifetime";
import { FrameError } from "./errors";

describe("acquired resource", () => {
  it("borrows the original resource without reading a wrapper's properties", async () => {
    const owner = new Lifetime("resource capture");
    class Resource {
      #closes = 0;
      get value(): never {
        throw new Error("the resource is opaque to its lifetime");
      }
      get closes() {
        return this.#closes;
      }
      close() {
        this.#closes++;
      }
    }
    const resource = new Resource();
    const close = resource.close;
    const acquired = await owner.acquire(
      "captured resource",
      () => resource,
      (value) => close.call(value),
    );
    let replacements = 0;
    resource.close = () => {
      replacements++;
    };
    try {
      await acquired.with((value) => expect(value).toBe(resource));
    } finally {
      await acquired.close();
      await owner.close();
    }
    expect(resource.closes).toBe(1);
    expect(replacements).toBe(0);
  });

  it("refuses an invalid release policy before creating a resource", async () => {
    const owner = new Lifetime("invalid release");
    let creates = 0;
    await expect(
      owner.acquire("invalid", () => creates++, undefined as never),
    ).rejects.toBeInstanceOf(TypeError);
    expect(creates).toBe(0);
    expect((await owner.close()).status).toBe("released");
  });

  it("keeps failed creation rollback with the creator and releases its capacity", async () => {
    const owner = new Lifetime("failed resource", {
      tasks: 1,
      resources: 1,
      children: 1,
    });
    const cause = new Error("creation failed");
    let closes = 0;
    await expect(
      owner.acquire(
        "failed",
        () => {
          throw cause;
        },
        () => {
          closes++;
        },
      ),
    ).rejects.toBe(cause);
    const next = await owner.acquire(
      "next",
      () => 42,
      () => {},
    );
    await expect(next.with((value) => value)).resolves.toBe(42);
    expect(closes).toBe(0);
    expect((await owner.close()).status).toBe("released");
  });

  it("owns undefined as a successful resource value", async () => {
    const owner = new Lifetime("undefined value");
    let closes = 0;
    const acquired = await owner.acquire(
      "undefined",
      () => undefined,
      (value) => {
        expect(value).toBeUndefined();
        closes++;
      },
    );
    await acquired.with((value) => expect(value).toBeUndefined());
    expect((await acquired.close()).status).toBe("released");
    expect(closes).toBe(1);
    await owner.close();
  });

  it("owns a late acquisition and rejects new borrowing atomically", async () => {
    const owner = new Lifetime("owner");
    const resource = Promise.withResolvers<number>();
    let closes = 0;
    const pending = owner.acquire(
      "late",
      () => resource.promise,
      (value) => {
        expect(value).toBe(42);
        closes++;
      },
    );
    const closed = owner.close();
    resource.resolve(42);
    await expect(pending).rejects.toThrow("arrived after");
    expect((await closed).status).toBe("released");
    expect(closes).toBe(1);
  });

  it("joins current borrows before releasing and shares one exit", async () => {
    const owner = new Lifetime("owner");
    let finish!: () => void;
    const order: string[] = [];
    const acquired = await owner.acquire(
      "value",
      () => 7,
      () => {
        order.push("closed");
      },
    );
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
    const resource = Promise.withResolvers<number>();
    const operation = owner.acquire(
      "late",
      () => resource.promise,
      () => ({ status: "released", error: new Error("cleanup failed") }),
    );
    const closing = owner.close();
    resource.resolve(42);
    await expect(operation).rejects.toMatchObject({
      errors: [
        { code: "closed" },
        { code: "close_failed", cause: { status: "released" } },
      ],
    });
    expect((await closing).status).toBe("released");
  });

  it.each(["parent", "handle"] as const)(
    "releases a late-confirmed payload when %s initiates close",
    async (initiator) => {
      const collect = (globalThis as { gc?: () => void }).gc;
      if (!collect)
        throw new Error("this required retention test needs Node --expose-gc");
      const { handle, payload, release } = await closedAcquired(initiator);
      for (let attempt = 0; attempt < 50; attempt++) {
        await new Promise((resolve) => setTimeout(resolve, 0));
        collect();
        await new Promise((resolve) => setTimeout(resolve, 0));
        if (!payload.deref() && !release.deref()) break;
      }
      expect(payload.deref()).toBeUndefined();
      expect(release.deref()).toBeUndefined();
      expect(() => handle.with(() => {})).toThrow("closed");
      const first = await handle.close();
      expect(first.status).toBe("released");
      expect((await handle.wait()).closeId).toBe(first.closeId);
    },
  );

  it("borrows ordinary results and closes resource before returning", async () => {
    const owner = new Lifetime("owner");
    let closed = false;
    const result = await owner.withResource(
      "value",
      () => 8,
      () => {
        closed = true;
      },
      (value) => value * 2,
    );
    expect(result).toBe(16);
    expect(closed).toBe(true);
    await owner.close();
  });

  it("reports bounded cleanup diagnostics even after confirmed release", async () => {
    const owner = new Lifetime("owner");
    const operation = owner.withResource(
      "value",
      () => 42,
      () => ({ status: "released", error: new Error("private detail") }),
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
    const operation = owner.withResource(
      "value",
      () => 42,
      () => ({ status: "released", error: new Error("cleanup rejected") }),
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

  it.each([new Error("work rejected"), undefined])(
    "returns the original work rejection %s when close adds no failure",
    async (primary) => {
      const owner = new Lifetime("owner");
      await expect(
        owner.withResource(
          "value",
          () => 1,
          () => {},
          () => {
            throw primary;
          },
        ),
      ).rejects.toBe(primary);
      expect((await owner.close()).status).toBe("released");
    },
  );

  it.each([false, true])(
    "retains unconfirmed evidence with work rejection=%s",
    async (failed) => {
      const owner = new Lifetime("owner");
      let confirm!: () => void;
      const primary = new Error("work rejected");
      const operation = owner.withResource(
        "value",
        (_signal, cleanup) => {
          confirm = () => cleanup.confirmReleased("termination observed");
          return 1;
        },
        () => ({ status: "unconfirmed" }),
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

async function closedAcquired(initiator: "parent" | "handle") {
  const owner = new Lifetime("owner");
  let confirmation!: Cleanup;
  let payload!: WeakRef<object>;
  const releasePayload = { bytes: new Uint8Array(1024 * 1024) };
  const handle = await owner.acquire(
    "late-confirmed value",
    (_signal, cleanup) => {
      confirmation = cleanup;
      const value = { bytes: new Uint8Array(1024 * 1024) };
      payload = new WeakRef(value);
      return value;
    },
    () => {
      expect(releasePayload.bytes.length).toBe(1024 * 1024);
      return { status: "unconfirmed" };
    },
  );
  const initial = await (initiator === "parent"
    ? owner.close()
    : handle.close());
  expect(initial.status).toBe("unconfirmed");
  confirmation.confirmReleased("original adapter observed termination");
  expect((await owner.wait()).status).toBe("released");
  return { handle, payload, release: new WeakRef(releasePayload) };
}
