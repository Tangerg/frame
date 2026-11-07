import { describe, expect, it, vi } from "vitest";
import { defineUnit, type Lifetime, type Unit } from "../index.js";
import {
  PROTOCOL,
  type CloseProof,
  type Fence,
  type LocalPlan,
  type Poll,
} from "../protocol.generated.js";
import type { HostControl } from "./control.js";
import { ControlPump, type PumpOptions } from "./pump.js";
import { Runtime } from "./runtime.js";

const authority = { id: "host", epoch: "1" };
const realm = { id: "window", epoch: "1" };
const digest = "a".repeat(64);

function plan(id: string): LocalPlan {
  return {
    protocol: PROTOCOL,
    approvalToken: id,
    authority,
    realm,
    execution: { id, generation: "1" },
    scopeId: id,
    scopeGeneration: "1",
    units: [
      {
        instance: {
          id,
          generation: "1",
          source: { installationId: id, release: digest, unitId: id },
          configuration: digest,
        },
        configurationJson: "{}",
        profile: "browser-js",
        module: { path: id, digest },
        imports: [],
        exports: [],
      },
    ],
  };
}

function fixture(units: readonly Unit[], preparationCapacity = 2) {
  const pending: { plans: LocalPlan[]; fences: Fence[] } = {
    plans: units.map((unit) => plan(unit.id)),
    fences: [],
  };
  const unexpected = async (): Promise<never> => {
    throw new Error("unexpected control operation");
  };
  const control: HostControl = {
    admitPlan: vi.fn<HostControl["admitPlan"]>(async (plan) => ({
      authority,
      realm,
      execution: plan.execution,
      decision: "admitted",
    })),
    ready: vi.fn<HostControl["ready"]>(async (report) => {
      pending.plans = pending.plans.filter(
        (plan) => plan.execution.id !== report.execution.id,
      );
      return { decision: "accepted" };
    }),
    poll: vi.fn<HostControl["poll"]>(async () => ({
      authority,
      realm,
      ...pending,
    })),
    prove: vi.fn<HostControl["prove"]>(async (proof) => {
      if (proof.scopeKind === "execution")
        pending.plans = pending.plans.filter(
          (plan) => plan.execution.id !== proof.execution.id,
        );
      return {
        closeId: proof.closeId,
        proofId: proof.proofId,
        recorded: true,
        authority,
      };
    }),
    reconnect: vi.fn<HostControl["reconnect"]>(async (request) => ({
      authority,
      realm,
      mode: "revalidated",
      executions: request.executions.map((execution) => ({
        execution,
        scopeId: execution.id,
        scopeGeneration: "1",
      })),
    })),
    acquire: unexpected,
    validateUse: unexpected,
    validateRoute: unexpected,
    snapshot: unexpected,
    closeSnapshot: unexpected,
    publish: unexpected,
    publication: unexpected,
    withdraw: unexpected,
  };
  const runtime = new Runtime({
    authority,
    realm,
    control,
    proofCapacity: 8,
    load: (plan) => {
      const unit = units.find((unit) => unit.id === plan.module.path);
      if (!unit) throw new Error("unknown approved unit");
      return unit;
    },
    remote: unexpected,
  });
  const onError = vi.fn<(error: unknown) => void>();
  const pump = new ControlPump(runtime, {
    intervalMilliseconds: 1,
    preparationCapacity,
    onError,
  });
  return { runtime, control, pump, pending, onError };
}

describe("control pump ownership", () => {
  it("captures prototype options once and invokes the original private-state handler", async () => {
    const failure = new Error("original setup failed");
    const f = fixture([
      defineUnit({
        id: "failed",
        imports: {},
        exports: {},
        setup() {
          throw failure;
        },
      }),
    ]);
    class Options implements PumpOptions {
      readonly reads = { interval: 0, capacity: 0, handler: 0 };
      readonly #failures: unknown[] = [];

      get intervalMilliseconds() {
        return ++this.reads.interval === 1 ? 1 : 0;
      }
      get preparationCapacity() {
        return ++this.reads.capacity === 1 ? 1 : 0;
      }
      get onError() {
        this.reads.handler++;
        return this.#report;
      }
      get failures() {
        return this.#failures;
      }
      #report(error: unknown) {
        this.#failures.push(error);
      }
    }
    const options = new Options();
    const pump = new ControlPump(f.runtime, options);
    const replacement = vi.fn();
    Object.defineProperty(options, "onError", { value: replacement });
    try {
      await pump.synchronize();
      await vi.waitFor(() => expect(options.failures).toEqual([failure]));
      expect(options.reads).toEqual({ interval: 1, capacity: 1, handler: 1 });
      expect(replacement).not.toHaveBeenCalled();
    } finally {
      await pump.close();
    }
  });

  it("joins the original poll when its control adapter reenters synchronization", async () => {
    const f = fixture([]);
    const poll = vi.mocked(f.control.poll).getMockImplementation()!;
    let reentered: Promise<void> | undefined;
    vi.mocked(f.control.poll).mockImplementationOnce(() => {
      reentered = f.pump.synchronize();
      return poll();
    });

    const polling = f.pump.synchronize();
    await polling;
    await reentered;
    expect(f.control.poll).toHaveBeenCalledOnce();
    expect(reentered).toBe(polling);
    await f.pump.close();
  });

  it("owns the running loop before its control adapter can start another", async () => {
    const f = fixture([]);
    const stop = new AbortController();
    const poll = vi.mocked(f.control.poll).getMockImplementation()!;
    let reentered: Promise<void> | undefined;
    vi.mocked(f.control.poll).mockImplementationOnce(() => {
      reentered = f.pump.run(stop.signal);
      void reentered.catch(() => {});
      return poll();
    });

    const running = f.pump.run(stop.signal);
    stop.abort();
    await running;
    await expect(reentered).rejects.toMatchObject({ code: "invalid_plan" });
  });

  it("joins the original shutdown when a resource abort listener reenters close", async () => {
    let reentered: Promise<void> | undefined;
    const f = fixture([
      defineUnit({
        id: "original",
        imports: {},
        exports: {},
        setup({ lifetime }) {
          lifetime.signal.addEventListener(
            "abort",
            () => {
              reentered = f.pump.close();
            },
            { once: true },
          );
          return {};
        },
      }),
    ]);
    await f.runtime.prepare(plan("original"));
    const closeRuntime = vi.spyOn(f.runtime, "close");

    const closing = f.pump.close();
    await closing;
    await reentered;
    expect(closeRuntime).toHaveBeenCalledOnce();
    expect(reentered).toBe(closing);
  });

  it("stops synchronization when its proof adapter closes the pump", async () => {
    const f = fixture([
      defineUnit({
        id: "original",
        imports: {},
        exports: {},
        setup: () => ({}),
      }),
    ]);
    const execution = await f.runtime.prepare(plan("original"));
    await execution.close();
    const prove = vi.mocked(f.control.prove).getMockImplementation()!;
    let closing: Promise<void> | undefined;
    vi.mocked(f.control.prove).mockImplementationOnce((proof) => {
      closing = f.pump.close();
      void closing.catch(() => {});
      return prove(proof);
    });

    await f.pump.synchronize();
    await closing;
    expect(f.control.poll).not.toHaveBeenCalled();
    expect(f.runtime.pendingProofs).toEqual([]);
  });

  it("joins a pending poll when its control adapter closes the pump", async () => {
    const f = fixture([]);
    const poll = vi.mocked(f.control.poll).getMockImplementation()!;
    let closing: Promise<void> | undefined;
    vi.mocked(f.control.poll).mockImplementationOnce(() => {
      closing = f.pump.close();
      void closing.catch(() => {});
      return poll();
    });

    await f.pump.synchronize();
    await closing;
    expect(f.control.poll).toHaveBeenCalledOnce();
    expect(f.runtime.connection).toBe("closed");
  });

  it("reports only the original setup failure while its proof awaits acknowledgement", async () => {
    const failure = new Error(
      "partial setup failed after reserving a resource",
    );
    const release = vi.fn();
    const setup = vi.fn(({ lifetime }: { lifetime: Lifetime }) => {
      lifetime.cleanup(release);
      throw failure;
    });
    const f = fixture([
      defineUnit({ id: "partial", imports: {}, exports: {}, setup }),
    ]);
    const received = Promise.withResolvers<void>();
    const acknowledge = Promise.withResolvers<void>();
    const prove = vi.mocked(f.control.prove).getMockImplementation()!;
    vi.mocked(f.control.prove).mockImplementationOnce(async (proof) => {
      received.resolve();
      await acknowledge.promise;
      return prove(proof);
    });

    await f.pump.synchronize();
    await received.promise;
    await f.pump.synchronize();
    await f.pump.synchronize();
    expect(f.onError.mock.calls).toEqual([[failure]]);
    expect(setup).toHaveBeenCalledOnce();
    expect(release).toHaveBeenCalledOnce();
    expect(f.control.admitPlan).toHaveBeenCalledOnce();

    acknowledge.resolve();
    await f.runtime.flushProofs();
    await f.pump.synchronize();
    await f.pump.close();
    expect(f.onError.mock.calls).toEqual([[failure]]);
    expect(f.control.ready).not.toHaveBeenCalled();
    expect(f.runtime.pendingProofs).toEqual([]);
  });

  it("delivers a later fence while the original setup is still pending", async () => {
    const entered = Promise.withResolvers<void>();
    let signal!: AbortSignal;
    const setup = vi.fn(({ lifetime }: { lifetime: Lifetime }) => {
      signal = lifetime.signal;
      entered.resolve();
      return new Promise<never>((_resolve, reject) =>
        signal.addEventListener("abort", () => reject(signal.reason), {
          once: true,
        }),
      );
    });
    const f = fixture([
      defineUnit({ id: "slow", imports: {}, exports: {}, setup }),
    ]);
    await f.pump.synchronize();
    await entered.promise;
    await f.pump.synchronize();
    expect(setup).toHaveBeenCalledOnce();
    f.pending.fences = [{ scopeId: "slow", scopeGeneration: "1" }];
    await f.pump.synchronize();
    expect(signal.aborted).toBe(true);
    await vi.waitFor(() => expect(f.control.prove).toHaveBeenCalledOnce());
    expect(f.control.ready).not.toHaveBeenCalled();
    await f.pump.close();
  });

  it("bounds pending preparations and leaves deferred plans with the Host", async () => {
    const first = Promise.withResolvers<Record<string, never>>();
    const second = Promise.withResolvers<Record<string, never>>();
    const firstSetup = vi.fn(() => first.promise);
    const secondSetup = vi.fn(() => second.promise);
    const f = fixture(
      [
        defineUnit({
          id: "first",
          imports: {},
          exports: {},
          setup: firstSetup,
        }),
        defineUnit({
          id: "second",
          imports: {},
          exports: {},
          setup: secondSetup,
        }),
      ],
      1,
    );
    await f.pump.synchronize();
    await vi.waitFor(() => expect(firstSetup).toHaveBeenCalledOnce());
    await f.pump.synchronize();
    expect(firstSetup).toHaveBeenCalledOnce();
    expect(secondSetup).not.toHaveBeenCalled();
    first.resolve({});
    await vi.waitFor(() => expect(f.control.ready).toHaveBeenCalledOnce());
    await f.pump.synchronize();
    await vi.waitFor(() => expect(secondSetup).toHaveBeenCalledOnce());
    second.resolve({});
    await vi.waitFor(() => expect(f.control.ready).toHaveBeenCalledTimes(2));
    await f.pump.close();
  });

  it("keeps polling fences while a proof acknowledgement is pending", async () => {
    const entered = Promise.withResolvers<void>();
    const acknowledgement = Promise.withResolvers<void>();
    let signal!: AbortSignal;
    const f = fixture([
      defineUnit({
        id: "released",
        imports: {},
        exports: {},
        setup: () => ({}),
      }),
      defineUnit({
        id: "slow",
        imports: {},
        exports: {},
        setup({ lifetime }) {
          signal = lifetime.signal;
          entered.resolve();
          return new Promise<never>((_resolve, reject) =>
            signal.addEventListener("abort", () => reject(signal.reason), {
              once: true,
            }),
          );
        },
      }),
    ]);
    const released = await f.runtime.prepare(plan("released"));
    await released.close();
    vi.mocked(f.control.prove).mockImplementationOnce(
      async (proof: CloseProof) => {
        await acknowledgement.promise;
        return {
          closeId: proof.closeId,
          proofId: proof.proofId,
          recorded: true,
          authority,
        };
      },
    );
    await f.pump.synchronize();
    await entered.promise;
    f.pending.fences = [{ scopeId: "slow", scopeGeneration: "1" }];
    await f.pump.synchronize();
    expect(signal.aborted).toBe(true);
    acknowledgement.resolve();
    await f.pump.close();
  });

  it("aborts setup on shutdown and joins its actual completion before proving exit", async () => {
    const entered = Promise.withResolvers<void>();
    const release = Promise.withResolvers<Record<string, never>>();
    let signal!: AbortSignal;
    const f = fixture([
      defineUnit({
        id: "slow",
        imports: {},
        exports: {},
        setup({ lifetime }) {
          signal = lifetime.signal;
          entered.resolve();
          return release.promise;
        },
      }),
    ]);
    const stop = new AbortController();
    const running = f.pump.run(stop.signal);
    await entered.promise;
    stop.abort();
    expect(signal.aborted).toBe(true);
    let joined = false;
    void running.then(() => {
      joined = true;
    });
    await Promise.resolve();
    expect(joined).toBe(false);
    expect(f.control.prove).not.toHaveBeenCalled();
    release.resolve({});
    await running;
    expect(f.control.ready).not.toHaveBeenCalled();
    expect(f.control.prove).toHaveBeenCalledOnce();
    expect(f.onError).not.toHaveBeenCalled();
  });

  it("retries failed shutdown delivery with the same original proof", async () => {
    const setup = vi.fn(() => ({}));
    const f = fixture([
      defineUnit({ id: "original", imports: {}, exports: {}, setup }),
    ]);
    await f.pump.synchronize();
    await vi.waitFor(() => expect(f.control.ready).toHaveBeenCalledOnce());
    vi.mocked(f.control.prove).mockRejectedValueOnce(
      new Error("proof acknowledgement lost"),
    );
    await expect(f.pump.close()).rejects.toThrow("shutdown did not complete");
    const proof = vi.mocked(f.control.prove).mock.calls[0]![0];
    await f.pump.close();
    expect(vi.mocked(f.control.prove).mock.calls[1]![0]).toEqual(proof);
    expect(setup).toHaveBeenCalledOnce();
    expect(f.runtime.pendingProofs).toEqual([]);
  });

  it.each(["reply", "failure"])(
    "ignores a retired channel's late poll %s after reconnect",
    async (outcome) => {
      const f = fixture([
        defineUnit({
          id: "current",
          imports: {},
          exports: {},
          setup: () => ({}),
        }),
      ]);
      const oldPoll = Promise.withResolvers<Poll>();
      vi.mocked(f.control.poll).mockImplementationOnce(() => oldPoll.promise);
      const polling = f.pump.synchronize();
      const current: HostControl = {
        ...f.control,
        poll: vi.fn<HostControl["poll"]>(async () => ({
          authority,
          realm,
          ...f.pending,
        })),
      };
      await f.runtime.reconnect(current);
      if (outcome === "reply")
        oldPoll.resolve({
          authority: { id: "retired", epoch: "0" },
          realm,
          plans: [],
          fences: [{ scopeId: "current", scopeGeneration: "1" }],
        });
      else oldPoll.reject(new Error("retired transport failed"));
      await polling;
      expect(f.runtime.connection).toBe("connected");
      await f.pump.synchronize();
      await vi.waitFor(() => expect(current.ready).toHaveBeenCalledOnce());
      expect(current.poll).toHaveBeenCalledOnce();
      expect(f.control.poll).toHaveBeenCalledOnce();
      expect(f.runtime.connection).toBe("connected");
      expect(f.onError).not.toHaveBeenCalled();
      await f.pump.close();
    },
  );
});
