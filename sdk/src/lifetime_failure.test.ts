import { describe, expect, it } from "vitest";
import { FrameError } from "./errors.js";
import { Lifetime, type CloseOutcome } from "./lifetime.js";

const diagnosticFailures = [
  {
    name: "prototype inspection",
    thrown: () =>
      new Proxy(
        {},
        {
          getPrototypeOf() {
            throw new Error("diagnostic inspection failed");
          },
        },
      ),
  },
  {
    name: "proxied error code",
    thrown: () =>
      new Proxy(new FrameError("capacity", "resource failed"), {
        get(target, key, receiver) {
          if (key === "code") throw new Error("diagnostic inspection failed");
          return Reflect.get(target, key, receiver);
        },
      }),
  },
  {
    name: "own error code",
    thrown: () =>
      Object.defineProperty(
        new FrameError("capacity", "resource failed"),
        "code",
        {
          get() {
            throw new Error("diagnostic inspection failed");
          },
        },
      ),
  },
  {
    name: "plain thrown value",
    thrown: () => ({
      get code() {
        throw new Error("diagnostic inspection failed");
      },
    }),
  },
];

describe.each(diagnosticFailures)(
  "bounded lifetime failures: $name",
  ({ thrown }) => {
    it("settles failed cleanup and retains the original late-release authority", async () => {
      const owner = new Lifetime("cleanup");
      let attempts = 0;
      const resource = owner.cleanup(() => {
        attempts++;
        throw thrown();
      }, "resource");
      const exit = owner.beginClose();
      expect(await exit.settled()).toMatchObject({
        status: "unconfirmed",
        pending: ["resource"],
        failures: [{ phase: "cleanup", category: "thrown_value" }],
      });
      resource.confirmReleased("original resource terminated");
      expect(await exit.wait()).toMatchObject({
        status: "released",
        pending: [],
      });
      expect(attempts).toBe(1);
    });

    it("joins a rejected task without creating a second rejection", async () => {
      const owner = new Lifetime("task");
      const error = thrown();
      await expect(
        owner.run(() => {
          throw error;
        }),
      ).rejects.toBe(error);
      expect(await owner.close()).toMatchObject({
        status: "released",
        failures: [{ phase: "task", category: "thrown_value" }],
      });
    });

    it("joins a child after its original resource confirms release", async () => {
      const parent = new Lifetime("parent");
      const child = parent.branch("child");
      const resource = child.cleanup(() => {
        throw thrown();
      }, "resource");
      expect((await parent.close()).status).toBe("unconfirmed");
      resource.confirmReleased("original resource terminated");
      expect(await parent.wait()).toMatchObject({
        status: "released",
        failures: [{ phase: "cleanup", category: "thrown_value" }],
      });
    });
  },
);

it("retains only recognized error codes after a mutable FrameError changes its code", async () => {
  const owner = new Lifetime("diagnostic categories");
  owner.cleanup(() => ({
    status: "released",
    error: Object.defineProperty(
      new FrameError("capacity", "resource failed"),
      "code",
      {
        value: { retained: new Uint8Array(1024) },
      },
    ),
  }));
  expect(await owner.close()).toMatchObject({
    status: "released",
    failures: [{ phase: "cleanup", category: "exception" }],
  });
});

it("preserves recognized FrameError categories without retaining their payloads", async () => {
  const owner = new Lifetime("known diagnostic");
  owner.cleanup(() => ({
    status: "released",
    error: new FrameError("capacity", "private diagnostic"),
  }));
  expect((await owner.close()).failures).toEqual([
    { phase: "cleanup", category: "capacity" },
  ]);
});

it("uses the exact lifetime limits captured before validation", async () => {
  let reads = 0;
  const owner = new Lifetime("captured limits", {
    get tasks() {
      return ++reads === 1 ? 1 : 0;
    },
    resources: 1,
    children: 1,
  });
  expect(await owner.run(() => 1)).toBe(1);
  expect(reads).toBe(1);
  expect((await owner.close()).status).toBe("released");
});

describe("exit observation", () => {
  const observers = [
    {
      name: "synchronous exception",
      fail(error: Error): unknown {
        throw error;
      },
    },
    {
      name: "async rejection",
      async fail(error: Error) {
        await Promise.resolve();
        throw error;
      },
    },
    {
      name: "thenable rejection",
      fail(error: Error): unknown {
        return new Proxy(
          {},
          {
            get(_target, key) {
              if (key !== "then") return undefined;
              return (_resolve: unknown, reject: (error: unknown) => void) => {
                reject(error);
                reject(new Error("duplicate settlement"));
                throw new Error("throw after settlement");
              };
            },
          },
        );
      },
    },
    {
      name: "throwing then getter",
      fail(error: Error): unknown {
        return new Proxy(
          {},
          {
            get() {
              throw error;
            },
          },
        );
      },
    },
  ];

  it.each(observers)(
    "isolates $name without changing release evidence",
    async ({ fail }) => {
      const owner = new Lifetime("observed resource");
      const failure = new Error("observer failed");
      const observations: CloseOutcome[] = [];
      const unhandled: unknown[] = [];
      const rejected = (error: unknown) => unhandled.push(error);
      process.on("unhandledRejection", rejected);
      try {
        const observe = (outcome: CloseOutcome) => {
          observations.push(outcome);
          return fail(failure);
        };
        owner.observeExit(observe);
        const exit = owner.beginClose();
        exit.subscribe(observe);
        const outcome = await owner.close();
        expect(() => owner.observeExit(observe)).not.toThrow();
        await new Promise<void>((resolve) => setImmediate(resolve));

        expect(unhandled).toEqual([]);
        expect(observations).toEqual([outcome, outcome, outcome]);
        expect(exit.outcome).toBe(outcome);
        expect(outcome).toMatchObject({ status: "released", failures: [] });
      } finally {
        process.off("unhandledRejection", rejected);
      }
    },
  );

  it("does not make pending observations a resource-release dependency", async () => {
    const owner = new Lifetime("independent observers");
    const pending = Promise.withResolvers<void>();
    owner.observeExit(() => pending.promise);
    const exit = owner.beginClose();
    exit.subscribe(() => pending.promise);

    const outcome = await owner.close();
    expect(outcome.status).toBe("released");
    pending.reject(new Error("observer finished after release"));
    await new Promise<void>((resolve) => setImmediate(resolve));
    expect(exit.outcome).toBe(outcome);
    expect(outcome.failures).toEqual([]);
  });
});
