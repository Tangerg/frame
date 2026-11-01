import { describe, expect, it } from "vitest";
import { Lifetime } from "./lifetime";

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void;
  const promise = new Promise<T>((yes) => {
    resolve = yes;
  });
  return { promise, resolve };
}

describe("structured lifetime", () => {
  it("registers work before reentrant close and joins it", async () => {
    const owner = new Lifetime("owner");
    const finished = deferred<number>();
    const order: string[] = [];
    owner.cleanup(() => {
      order.push("cleanup");
    });
    const task = owner.run(() => {
      owner.beginClose();
      order.push("task");
      return finished.promise;
    });
    expect(() => owner.run(() => 2)).toThrow("no longer accepts");
    await Promise.resolve();
    expect(order).toEqual(["task"]);
    finished.resolve(3);
    expect(await task).toBe(3);
    expect((await owner.wait()).status).toBe("released");
    expect(order).toEqual(["task", "cleanup"]);
  });

  it("preserves earlier resources until late proof and never replays cleanup", async () => {
    const owner = new Lifetime("owner");
    const order: string[] = [];
    owner.cleanup(() => {
      order.push("provider");
    }, "provider");
    const failed = owner.cleanup(() => {
      order.push("consumer");
      throw new Error("resource remains alive");
    }, "consumer");
    const exit = owner.beginClose();
    const first = await owner.close();
    expect(first.status).toBe("unconfirmed");
    expect(first.pending).toEqual(["provider", "consumer"]);
    expect(first.failures).toEqual([
      { phase: "cleanup", category: "exception" },
    ]);
    expect(owner.beginClose()).toBe(exit);
    expect(order).toEqual(["consumer"]);
    failed.confirmReleased("original-resource:terminated");
    expect((await exit.wait()).status).toBe("released");
    expect(first.status).toBe("unconfirmed");
    expect(order).toEqual(["consumer", "provider"]);
    await owner.close();
    failed.confirmReleased("original-resource:terminated");
    expect(order).toEqual(["consumer", "provider"]);
  });

  it("finishes independent branches while retaining a failed branch and parent dependencies", async () => {
    const owner = new Lifetime("owner");
    const order: string[] = [];
    owner.cleanup(() => {
      order.push("parent-provider");
    });
    const first = owner.branch("first");
    const second = owner.branch("second");
    const resource = first.cleanup(
      () => ({ status: "unconfirmed" }),
      "first-resource",
    );
    second.cleanup(() => {
      order.push("second");
    });
    expect((await owner.close()).status).toBe("unconfirmed");
    expect(order).toEqual(["second"]);
    resource.confirmReleased("late-release");
    expect((await owner.wait()).status).toBe("released");
    expect(order).toEqual(["second", "parent-provider"]);
  });

  it("broadcasts cancellation before waiting and does not cancel exit with a waiter", async () => {
    const owner = new Lifetime("owner");
    const a = owner.branch("a");
    const b = owner.branch("b");
    const gate = deferred<void>();
    const observed: string[] = [];
    a.run((signal) => {
      signal.addEventListener("abort", () => {
        observed.push("a");
      });
      return gate.promise;
    });
    b.run((signal) => {
      signal.addEventListener("abort", () => {
        observed.push("b");
      });
      return gate.promise;
    });
    const waiter = new AbortController();
    const waiting = owner.wait(waiter.signal);
    expect(observed).toEqual(["a", "b"]);
    waiter.abort(new Error("stop observing"));
    await expect(waiting).rejects.toThrow("stop observing");
    gate.resolve();
    expect((await owner.wait()).status).toBe("released");
  });

  it("separates released resources with errors from unconfirmed release", async () => {
    const owner = new Lifetime("owner");
    owner.cleanup(() => ({
      status: "released",
      evidence: "closed",
      error: new Error("post-close reporting failed"),
    }));
    const report = await owner.close();
    expect(report.status).toBe("released");
    expect(report.pending).toEqual([]);
    expect(report.failures).toEqual([
      { phase: "cleanup", category: "exception" },
    ]);
  });

  it("retains only actual provider dependencies when one consumer cannot exit", async () => {
    const provider = new Lifetime("provider");
    const consumer = new Lifetime("consumer");
    const independent = new Lifetime("independent");
    consumer.dependsOn(provider);
    const order: string[] = [];
    provider.cleanup(() => {
      order.push("provider");
    });
    independent.cleanup(() => {
      order.push("independent");
    });
    const held = consumer.cleanup(() => ({ status: "unconfirmed" }));
    consumer.beginClose();
    provider.beginClose();
    independent.beginClose();
    expect((await provider.close()).status).toBe("unconfirmed");
    expect((await independent.wait()).status).toBe("released");
    expect(order).toEqual(["independent"]);
    held.confirmReleased("original owner released the resource");
    await provider.wait();
    expect(order).toEqual(["independent", "provider"]);
  });

  it("refuses dependencies that would deadlock an ancestor's exit", async () => {
    const parent = new Lifetime("parent");
    const child = parent.branch("child");
    const outside = new Lifetime("outside");
    expect(() => parent.dependsOn(child)).toThrow("dependency cycle");
    parent.dependsOn(outside);
    expect(() => outside.dependsOn(child)).toThrow("dependency cycle");
    const outcomes = await Promise.all([parent.wait(), outside.wait()]);
    expect(outcomes.map((outcome) => outcome.status)).toEqual([
      "released",
      "released",
    ]);
  });

  it("rejects capacity before dispatch without evicting unresolved work", async () => {
    const owner = new Lifetime("owner", {
      tasks: 1,
      resources: 1,
      children: 1,
    });
    const gate = deferred<void>();
    owner.run(() => gate.promise);
    expect(() =>
      owner.run(() => {
        throw new Error("not dispatched");
      }),
    ).toThrow("capacity");
    owner.cleanup(() => ({ status: "unconfirmed" }));
    expect(() => owner.cleanup(() => {})).toThrow("capacity");
    gate.resolve();
    expect((await owner.close()).status).toBe("unconfirmed");
  });
});
