import { describe, expect, it, vi } from "vitest";
import {
  contribution,
  FrameError,
  Lifetime,
  optional,
  defineUnit,
  Event,
  Point,
  Service,
  type Cleanup,
  type Unit,
} from "../index.js";
import {
  PROTOCOL,
  type CloseProof,
  type ContractRef,
  type LocalPlan,
  type PlanAdmission,
  type PlannedUnit,
  type Poll,
  type PublicationRequest,
  type PublicationResult,
  type ReadyReport,
  type UseGrant,
} from "../protocol.generated.js";
import { Runtime } from "./runtime";
import type { HostControl } from "./control";
import { HTTPControl } from "./http";

const digest = "a".repeat(64);
const authority = { id: "host", epoch: "host-1" };
const realm = { id: "window", epoch: "window-1" };
const ref = (
  name: string,
  kind: ContractRef["kind"] = "service",
): ContractRef => ({ name, version: "1", digest, kind, mode: "native" });
const files = new Service<{ read(): string }>(ref("documents.files"));
const preview = new Service<{ text(): string }>(ref("documents.preview"));
const additions = new Point<{ title: string }, { value: string }>(
  ref("documents.additions", "point"),
);

function unit(
  id: string,
  name: string,
  exports: PlannedUnit["exports"],
  imports: PlannedUnit["imports"] = [],
  value = id,
): PlannedUnit {
  return {
    instance: {
      id,
      generation: "1",
      source: { installationId: id, release: digest, unitId: name },
      configuration: digest,
    },
    configurationJson: JSON.stringify(value),
    profile: "browser-js",
    module: { path: name, digest },
    imports,
    exports,
  };
}

function plan(id: string, units: readonly PlannedUnit[]): LocalPlan {
  return {
    protocol: PROTOCOL,
    approvalToken: `plan:${id}`,
    authority,
    realm,
    execution: { id, generation: "1" },
    scopeId: id,
    scopeGeneration: "1",
    units,
  };
}

function fixture(definitions: Record<string, Unit>, proofCapacity = 128) {
  const ready = new Map<string, ReadyReport>();
  const grants = new Map<string, UseGrant>();
  const publications = new Map<string, PublicationResult>();
  const proofs = new Map<string, CloseProof>();
  let nextGrant = 0;
  const control: HostControl = {
    admitPlan: vi.fn<HostControl["admitPlan"]>(async (value) => {
      if (value.approvalToken !== `plan:${value.execution.id}`)
        throw new Error("invalid plan approval");
      return {
        authority,
        realm,
        execution: value.execution,
        decision: "admitted",
      };
    }),
    ready: vi.fn<HostControl["ready"]>(async (report) => {
      ready.set(report.execution.id, report);
      return { decision: "accepted" };
    }),
    acquire: vi.fn<HostControl["acquire"]>(async () => {
      throw new Error("fixture requires an explicit grant");
    }),
    validateUse: vi.fn<HostControl["validateUse"]>(async (grant) => {
      if (JSON.stringify(grants.get(grant.id)) !== JSON.stringify(grant))
        throw new FrameError("stale_binding", "invalid use approval");
    }),
    validateRoute: vi.fn<HostControl["validateRoute"]>(async () => {}),
    snapshot: vi.fn<HostControl["snapshot"]>(async () => ({
      id: "snapshot",
      authority,
      points: [],
    })),
    closeSnapshot: vi.fn<HostControl["closeSnapshot"]>(async () => {}),
    publish: vi.fn<HostControl["publish"]>(async (request) => {
      const result: PublicationResult = {
        decision: "accepted",
        publication: {
          id: `publication:${request.operationId}`,
          operationId: request.operationId,
          revision: "1",
          object: request.object,
        },
      };
      publications.set(request.operationId, result);
      return result;
    }),
    publication: vi.fn<HostControl["publication"]>(
      async (id) => publications.get(id) ?? { decision: "unknown" },
    ),
    withdraw: vi.fn<HostControl["withdraw"]>(async () => ({
      decision: "accepted",
    })),
    prove: vi.fn<HostControl["prove"]>(async (proof) => {
      const previous = proofs.get(proof.proofId);
      if (previous && JSON.stringify(previous) !== JSON.stringify(proof))
        throw new Error("proof changed");
      proofs.set(proof.proofId, proof);
      if (proof.useId) grants.delete(proof.useId);
      return {
        closeId: proof.closeId,
        proofId: proof.proofId,
        recorded: true,
        authority,
      };
    }),
    poll: vi.fn<HostControl["poll"]>(async () => ({
      authority,
      realm,
      plans: [],
      fences: [],
    })),
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
  };
  const runtime = new Runtime({
    authority,
    realm,
    control,
    proofCapacity,
    load: (entry) => definitions[entry.module.path]!,
    remote: () => {
      throw new Error("unexpected remote import");
    },
  });
  function grant(
    id: string,
    objects = ready.get(id)!.objects,
    operationId = `operation:${nextGrant + 1}`,
    parentUseId?: string,
  ): UseGrant {
    const value: UseGrant = {
      id: `use:${++nextGrant}`,
      operationId,
      ...(parentUseId ? { parentUseId } : {}),
      approvalToken: `grant:${nextGrant}`,
      authority,
      realm,
      execution: { id, generation: "1" },
      scopeId: id,
      scopeGeneration: "1",
      objects,
    };
    grants.set(value.id, value);
    return value;
  }
  return { runtime, control, ready, grants, grant, publications, proofs };
}

it.each(["load", "remote"] as const)(
  "joins the unit's pending %s callback before releasing its resources",
  async (phase) => {
    const started = Promise.withResolvers<void>();
    const finish = Promise.withResolvers<void>();
    const released = vi.fn();
    const dependency = new Service({ ...files.ref, mode: "portable" });
    const definition = defineUnit({
      id: "consumer",
      imports: { dependency },
      exports: {},
      setup: vi.fn(() => ({})),
    });
    const { control } = fixture({});
    const prepareResource = async (owner: Lifetime) => {
      owner.cleanup(released, "preparing resource");
      started.resolve();
      await finish.promise;
    };
    const runtime = new Runtime({
      authority,
      realm,
      control,
      proofCapacity: 2,
      load: async (_unit, owner) => {
        if (phase === "load") await prepareResource(owner);
        return definition;
      },
      remote: async (_binding, owner) => {
        if (phase === "remote") await prepareResource(owner);
        return {};
      },
    });
    const preparing = runtime.prepare(
      plan("original", [
        unit(
          "consumer",
          "consumer",
          [],
          [
            {
              slot: "dependency",
              kind: "remote",
              contract: dependency.ref,
              remote: {
                token: "approved",
                source: {
                  installationId: "provider",
                  release: digest,
                  unitId: "provider",
                  export: "dependency",
                  contract: dependency.ref,
                },
              },
            },
          ],
        ),
      ]),
    );
    const rejected = expect(preparing).rejects.toMatchObject({
      code: "unavailable",
    });
    await started.promise;
    const closing = runtime.close();
    try {
      await new Promise<void>((resolve) => setImmediate(resolve));
      expect(released).not.toHaveBeenCalled();
      expect(control.prove).not.toHaveBeenCalled();
    } finally {
      finish.resolve();
      await rejected;
      await closing;
    }
    expect(released).toHaveBeenCalledOnce();
    expect(definition.setup).not.toHaveBeenCalled();
    expect(control.prove).toHaveBeenCalledOnce();
  },
);

const reader = defineUnit({
  id: "reader",
  imports: {},
  exports: { files },
  setup: ({ configuration }) => ({
    files: { read: () => configuration as string },
  }),
});
const viewer = defineUnit({
  id: "viewer",
  imports: { source: files, destination: files },
  exports: { preview },
  setup: ({ imports }) => ({
    preview: {
      text: () => `${imports.source.read()}:${imports.destination.read()}`,
    },
  }),
});

function previewPlan(id: string): LocalPlan {
  return plan(id, [
    unit(
      `${id}-view`,
      "viewer",
      [{ name: "preview", contract: preview.ref }],
      [
        {
          slot: "source",
          kind: "local",
          contract: files.ref,
          local: {
            instanceId: `${id}-source`,
            name: "files",
            contract: files.ref,
          },
        },
        {
          slot: "destination",
          kind: "local",
          contract: files.ref,
          local: {
            instanceId: `${id}-destination`,
            name: "files",
            contract: files.ref,
          },
        },
      ],
    ),
    unit(
      `${id}-source`,
      "reader",
      [{ name: "files", contract: files.ref }],
      [],
      `${id}:source`,
    ),
    unit(
      `${id}-destination`,
      "reader",
      [{ name: "files", contract: files.ref }],
      [],
      `${id}:destination`,
    ),
  ]);
}

describe("approved browser execution", () => {
  it("installs the original admission before a control adapter can reenter preparation", async () => {
    const setup = vi.fn(() => ({}));
    const f = fixture({
      owned: defineUnit({ id: "owned", imports: {}, exports: {}, setup }),
    });
    const original = plan("original", [unit("original-unit", "owned", [])]);
    let reentered: Promise<unknown> | undefined;
    vi.mocked(f.control.admitPlan).mockImplementationOnce(async () => {
      reentered = f.runtime.prepare(original);
      return {
        authority,
        realm,
        execution: original.execution,
        decision: "admitted",
      };
    });
    const execution = await f.runtime.prepare(original);
    expect(await reentered).toBe(execution);
    expect(f.control.admitPlan).toHaveBeenCalledOnce();
    expect(setup).toHaveBeenCalledOnce();
    await f.runtime.close();
  });

  it("cannot settle the pending owner with another execution's admission result", async () => {
    const setup = vi.fn(() => ({}));
    const f = fixture(
      { owned: defineUnit({ id: "owned", imports: {}, exports: {}, setup }) },
      1,
    );
    const original = plan("original", [unit("original-unit", "owned", [])]);
    vi.mocked(f.control.admitPlan).mockResolvedValueOnce({
      authority,
      realm,
      execution: { id: "different", generation: "1" },
      decision: "notAdmitted",
    });
    await expect(f.runtime.prepare(original)).rejects.toMatchObject({
      code: "outcome_unknown",
    });
    await expect(
      f.runtime.prepare(plan("other", [unit("other-unit", "owned", [])])),
    ).rejects.toMatchObject({ code: "capacity" });
    vi.mocked(f.control.admitPlan).mockResolvedValueOnce({
      authority,
      realm,
      execution: original.execution,
      decision: "cleanupRequired",
    });
    await f.runtime.close();
    expect(f.proofs.size).toBe(1);
    expect(setup).not.toHaveBeenCalled();
  });

  it.each(["notAdmitted", "settled"] as const)(
    "cancels the unstarted reservation after the Host reports %s",
    async (decision) => {
      const setup = vi.fn(() => ({}));
      const definition = defineUnit({
        id: "owned",
        imports: {},
        exports: {},
        setup,
      });
      const f = fixture({ owned: definition }, 1);
      const original = plan("original", [unit("original-unit", "owned", [])]);
      vi.mocked(f.control.admitPlan).mockResolvedValueOnce({
        authority,
        realm,
        execution: original.execution,
        decision,
      });
      await expect(f.runtime.prepare(original)).rejects.toMatchObject({
        code: "stale_binding",
      });
      expect(setup).not.toHaveBeenCalled();
      expect(f.runtime.pendingProofs).toEqual([]);
      await f.runtime.prepare(plan("next", [unit("next-unit", "owned", [])]));
      expect(setup).toHaveBeenCalledOnce();
      await f.runtime.close();
      expect(f.proofs.size).toBe(1);
      expect([...f.proofs.values()][0]!.execution.id).toBe("next");
    },
  );

  it("owns an admission before dispatch and joins its late result during shutdown", async () => {
    const setup = vi.fn(() => ({}));
    const f = fixture({
      owned: defineUnit({ id: "owned", imports: {}, exports: {}, setup }),
    });
    const original = plan("original", [unit("original-unit", "owned", [])]);
    const admission = Promise.withResolvers<PlanAdmission>();
    vi.mocked(f.control.admitPlan).mockImplementationOnce(
      () => admission.promise,
    );
    const preparing = f.runtime.prepare(original).catch((error) => error);
    const closing = f.runtime.close();
    let settled = false;
    void closing.then(() => {
      settled = true;
    });
    await Promise.resolve();
    expect(settled).toBe(false);
    expect(setup).not.toHaveBeenCalled();
    expect(f.proofs.size).toBe(0);
    admission.resolve({
      authority,
      realm,
      execution: original.execution,
      decision: "admitted",
    });
    await preparing;
    expect(
      (await closing).every((outcome) => outcome.status === "released"),
    ).toBe(true);
    expect(setup).not.toHaveBeenCalled();
    expect(f.proofs.size).toBe(1);
  });

  it.each(["notAdmitted", "cleanupRequired"] as const)(
    "reconciles an unknown admission as %s without starting setup",
    async (decision) => {
      const setup = vi.fn(() => ({}));
      const f = fixture(
        { owned: defineUnit({ id: "owned", imports: {}, exports: {}, setup }) },
        1,
      );
      const original = plan("original", [unit("original-unit", "owned", [])]);
      vi.mocked(f.control.admitPlan).mockRejectedValueOnce(
        new FrameError("outcome_unknown", "admission acknowledgement lost"),
      );
      await expect(f.runtime.prepare(original)).rejects.toMatchObject({
        code: "outcome_unknown",
      });
      await expect(
        f.runtime.prepare(plan("other", [unit("other-unit", "owned", [])])),
      ).rejects.toMatchObject({ code: "capacity" });
      expect(f.runtime.pendingProofs).toEqual([]);
      vi.mocked(f.control.admitPlan).mockResolvedValueOnce({
        authority,
        realm,
        execution: original.execution,
        decision,
      });
      expect(
        (await f.runtime.close()).every(
          (outcome) => outcome.status === "released",
        ),
      ).toBe(true);
      expect(f.proofs.size).toBe(decision === "cleanupRequired" ? 1 : 0);
      expect(setup).not.toHaveBeenCalled();
      expect(vi.mocked(f.control.admitPlan).mock.calls).toEqual([
        [original],
        [original],
      ]);
    },
  );

  it("retains an unknown admission after an ordinary refusal and retries original cleanup", async () => {
    const setup = vi.fn(() => ({}));
    const f = fixture({
      owned: defineUnit({ id: "owned", imports: {}, exports: {}, setup }),
    });
    const original = plan("original", [unit("original-unit", "owned", [])]);
    vi.mocked(f.control.admitPlan).mockRejectedValue(
      new FrameError(
        "permission_denied",
        "old credential cannot observe original admission",
      ),
    );
    await expect(f.runtime.prepare(original)).rejects.toMatchObject({
      code: "permission_denied",
    });
    expect((await f.runtime.close())[0]!.status).toBe("unconfirmed");
    expect(f.proofs.size).toBe(0);
    vi.mocked(f.control.admitPlan).mockResolvedValue({
      authority,
      realm,
      execution: original.execution,
      decision: "cleanupRequired",
    });
    expect((await f.runtime.close())[0]!.status).toBe("released");
    expect(setup).not.toHaveBeenCalled();
    expect(f.proofs.size).toBe(1);
  });

  it("replaces a closed Runtime's cleanup channel without reopening admission", async () => {
    const setup = vi.fn(() => ({}));
    const f = fixture({
      owned: defineUnit({ id: "owned", imports: {}, exports: {}, setup }),
    });
    const original = plan("original", [unit("original-unit", "owned", [])]);
    vi.mocked(f.control.admitPlan).mockRejectedValue(
      new FrameError("outcome_unknown", "original host unavailable"),
    );
    await expect(f.runtime.prepare(original)).rejects.toMatchObject({
      code: "outcome_unknown",
    });
    expect((await f.runtime.close())[0]!.status).toBe("unconfirmed");
    const current = { ...authority, epoch: "next-host" };
    const cleanup: HostControl = {
      ...f.control,
      reconnect: async () => ({
        authority: current,
        realm,
        mode: "cleanupOnly",
        executions: [],
      }),
      admitPlan: async (plan) => ({
        authority: current,
        realm,
        execution: plan.execution,
        decision: "cleanupRequired",
      }),
      prove: vi.fn(async (proof) => ({
        authority: current,
        closeId: proof.closeId,
        proofId: proof.proofId,
        recorded: true,
      })),
    };
    await f.runtime.reconnect(cleanup);
    expect(f.runtime.connection).toBe("closed");
    expect((await f.runtime.close())[0]!.status).toBe("released");
    expect(vi.mocked(cleanup.prove).mock.calls[0]![0].authority).toEqual(
      authority,
    );
    expect(setup).not.toHaveBeenCalled();
    await expect(
      f.runtime.prepare(plan("new", [unit("new-unit", "owned", [])])),
    ).rejects.toMatchObject({ code: "unavailable" });
  });

  it("retains one exit owner when approved preparation exceeds the local child budget", async () => {
    const f = fixture({ reader });
    const overflow = plan(
      "large",
      Array.from({ length: 257 }, (_, index) =>
        unit(`source-${index}`, "reader", [
          { name: "files", contract: files.ref },
        ]),
      ),
    );
    await expect(f.runtime.prepare(overflow)).rejects.toMatchObject({
      code: "capacity",
    });
    const reload = vi.fn();
    await f.runtime.reload(reload);
    expect(reload).toHaveBeenCalledOnce();
    expect(f.proofs.size).toBe(1);
    expect([...f.proofs.values()][0]!.instances).toHaveLength(257);
  });
  it("keeps two plans and two same-contract slots exact without per-call RPC", async () => {
    const f = fixture({ reader, viewer });
    const a = await f.runtime.prepare(previewPlan("a"));
    const b = await f.runtime.prepare(previewPlan("b"));
    const aRef = a.objects.find((entry) => entry.export.name === "preview")!;
    const bRef = b.objects.find((entry) => entry.export.name === "preview")!;
    const aUse = await f.runtime.accept(f.grant("a", [aRef]));
    const bUse = await f.runtime.accept(f.grant("b", [bRef]));
    for (let i = 0; i < 20; i++)
      expect(await aUse.with(aRef, preview, (value) => value.text())).toBe(
        "a:source:a:destination",
      );
    expect(f.control.validateUse).toHaveBeenCalledTimes(2);
    expect(f.control.acquire).not.toHaveBeenCalled();
    await aUse.close();
    expect((await a.close()).status).toBe("released");
    expect(await bUse.with(bRef, preview, (value) => value.text())).toBe(
      "b:source:b:destination",
    );
    await f.runtime.close();
  });

  it("rejects fake grants, wrong realms, relabeling and dependency cycles before invoking setup", async () => {
    const f = fixture({ reader, viewer });
    const first = previewPlan("a");
    const live = await f.runtime.prepare(first);
    await expect(
      f.runtime.prepare({ ...first, scopeGeneration: "2" }),
    ).rejects.toThrow("relabeled");
    await expect(
      f.runtime.prepare({
        ...previewPlan("b"),
        realm: { ...realm, id: "another-window" },
      }),
    ).rejects.toThrow("another authority or realm");
    const grant = f.grant("a", [live.objects[0]!]);
    await expect(
      f.runtime.accept({ ...grant, approvalToken: "forged" }),
    ).rejects.toThrow("invalid use");
    const recursive = unit(
      "cycle",
      "reader",
      [{ name: "files", contract: files.ref }],
      [
        {
          slot: "self",
          contract: files.ref,
          kind: "local",
          local: { instanceId: "cycle", name: "files", contract: files.ref },
        },
      ],
    );
    await expect(f.runtime.prepare(plan("cycle", [recursive]))).rejects.toThrow(
      "dependency cycle",
    );
    await f.runtime.close();
  });

  it("accepts an exact Host Use after ready committed but its ACK was lost", async () => {
    const f = fixture({ reader });
    vi.mocked(f.control.ready)
      .mockImplementationOnce(async (report) => {
        f.ready.set(report.execution.id, report);
        throw new Error("ACK lost");
      })
      .mockRejectedValueOnce(new FrameError("capacity", "recovery capacity"));
    await expect(
      f.runtime.prepare(
        plan("a", [
          unit("source", "reader", [{ name: "files", contract: files.ref }]),
        ]),
      ),
    ).rejects.toMatchObject({ code: "outcome_unknown" });
    const reference = f.ready.get("a")!.objects[0]!;
    const use = await f.runtime.accept(f.grant("a", [reference]));
    expect(await use.with(reference, files, (value) => value.read())).toBe(
      "source",
    );
    const original = f.ready.get("a")!;
    await expect(
      f.runtime.prepare(
        plan("a", [
          unit("source", "reader", [{ name: "files", contract: files.ref }]),
        ]),
      ),
    ).rejects.toMatchObject({ code: "outcome_unknown" });
    expect(await use.with(reference, files, (value) => value.read())).toBe(
      "source",
    );
    await f.runtime.prepare(
      plan("a", [
        unit("source", "reader", [{ name: "files", contract: files.ref }]),
      ]),
    );
    expect(f.ready.get("a")).toEqual(original);
    expect(vi.mocked(f.control.ready).mock.calls).toEqual([
      [original],
      [original],
      [original],
    ]);
    await f.runtime.close();
  });

  it("retains an exact provider in another plan until its approved consumer exits", async () => {
    const released = vi.fn();
    const provider = defineUnit({
      id: "reader",
      imports: {},
      exports: { files },
      setup({ lifetime }) {
        lifetime.cleanup(released);
        return { files: { read: () => "shared" } };
      },
    });
    const consumer = defineUnit({
      id: "consumer",
      imports: { source: files },
      exports: { preview },
      setup({ imports }) {
        return { preview: { text: () => imports.source.read() } };
      },
    });
    const f = fixture({ reader: provider, consumer });
    const a = await f.runtime.prepare(
      plan("provider", [
        unit("source", "reader", [{ name: "files", contract: files.ref }]),
      ]),
    );
    const b = await f.runtime.prepare(
      plan("consumer", [
        unit(
          "view",
          "consumer",
          [{ name: "preview", contract: preview.ref }],
          [
            {
              slot: "source",
              kind: "local",
              contract: files.ref,
              local: {
                instanceId: "source",
                name: "files",
                contract: files.ref,
              },
            },
          ],
        ),
      ]),
    );
    const use = await f.runtime.accept(f.grant("consumer", b.objects));
    expect((await a.close()).status).toBe("unconfirmed");
    expect(released).not.toHaveBeenCalled();
    expect(
      await use.with(b.objects[0]!, preview, (value) => value.text()),
    ).toBe("shared");
    await b.close();
    expect((await a.wait()).status).toBe("released");
    expect(released).toHaveBeenCalledOnce();
    await f.runtime.close();
  });

  it("settles a late authenticated admission without dispatching its object", async () => {
    const f = fixture({ reader });
    const live = await f.runtime.prepare(
      plan("a", [
        unit("source", "reader", [{ name: "files", contract: files.ref }]),
      ]),
    );
    const grant = f.grant("a", live.objects, "late");
    let resolve!: (value: UseGrant) => void;
    vi.mocked(f.control.acquire).mockImplementation(
      () =>
        new Promise((yes) => {
          resolve = yes;
        }),
    );
    const admitted = f.runtime.acquire({
      operationId: "late",
      snapshotId: "snapshot",
      execution: live.identity,
      selections: [],
    });
    const rejected = expect(admitted).rejects.toMatchObject({
      code: "unavailable",
    });
    f.runtime.disconnect();
    resolve(grant);
    await rejected;
    await f.runtime.close();
    expect(
      [...f.proofs.values()].find((proof) => proof.useId === grant.id),
    ).toMatchObject({
      status: "released",
      instances: ["source"],
      scopeGeneration: "1",
    });
  });

  it("retries an uncertain acquisition with its original operation and no second resource owner", async () => {
    const f = fixture({ reader });
    const live = await f.runtime.prepare(
      plan("a", [
        unit("source", "reader", [{ name: "files", contract: files.ref }]),
      ]),
    );
    const grant = f.grant("a", live.objects, "one-admission");
    vi.mocked(f.control.acquire)
      .mockRejectedValueOnce(new Error("lost reply"))
      .mockRejectedValueOnce(new FrameError("capacity", "receipt capacity"))
      .mockRejectedValueOnce(
        new FrameError("unavailable", "receipt recovery unavailable"),
      )
      .mockResolvedValue(grant);
    const request = {
      operationId: "one-admission",
      snapshotId: "snapshot",
      execution: live.identity,
      selections: [],
    };
    await expect(f.runtime.acquire(request)).rejects.toThrow("lost reply");
    expect(f.runtime.hasPendingAcquisition(request.operationId)).toBe(true);
    await expect(
      f.runtime.acquire({ ...request, snapshotId: "changed" }),
    ).rejects.toMatchObject({ code: "invalid_plan" });
    for (const code of ["capacity", "unavailable"]) {
      await expect(f.runtime.acquire(request)).rejects.toMatchObject({ code });
      expect(f.runtime.hasPendingAcquisition(request.operationId)).toBe(true);
    }
    const use = await f.runtime.acquire(request);
    expect(f.runtime.hasPendingAcquisition(request.operationId)).toBe(false);
    expect(await f.runtime.acquire(request)).toBe(use);
    expect(await f.runtime.accept(grant)).toBe(use);
    expect(vi.mocked(f.control.acquire).mock.calls).toEqual(
      Array.from({ length: 4 }, () => [request]),
    );
    expect(f.control.validateUse).toHaveBeenCalledOnce();
    expect(
      await use.with(live.objects[0]!, files, (value) => value.read()),
    ).toBe("source");
    await f.runtime.reload(() => {});
    expect(
      [...f.proofs.values()].filter((proof) => proof.useId === grant.id),
    ).toHaveLength(1);
  });

  it.each(["acquire", "accept"] as const)(
    "retains an acknowledged acquisition through %s when current authority validation fails",
    async (entrance) => {
      const f = fixture({ reader });
      const live = await f.runtime.prepare(
        plan("a", [
          unit("source", "reader", [{ name: "files", contract: files.ref }]),
        ]),
      );
      const grant = f.grant("a", live.objects, "accepted-admission");
      vi.mocked(f.control.acquire).mockResolvedValue(grant);
      vi.mocked(f.control.validateUse).mockRejectedValueOnce(
        new FrameError("capacity", "validation admission unavailable"),
      );
      const request = {
        operationId: grant.operationId,
        snapshotId: "snapshot",
        execution: live.identity,
        selections: [],
      };
      const accept = () =>
        entrance === "acquire"
          ? f.runtime.acquire(request)
          : f.runtime.accept(grant);
      await expect(accept()).rejects.toMatchObject({
        code: "capacity",
      });
      expect(f.runtime.hasPendingAcquisition(request.operationId)).toBe(true);
      const use = await accept();
      expect(await f.runtime.accept(grant)).toBe(use);
      expect(vi.mocked(f.control.acquire).mock.calls).toEqual(
        entrance === "acquire" ? [[request], [request]] : [],
      );
      await f.runtime.close();
      expect(
        [...f.proofs.values()].filter((proof) => proof.useId === grant.id),
      ).toHaveLength(1);
    },
  );

  it.each(["acquire", "accept"] as const)(
    "joins delegated child work before the parent proof through %s",
    async (entrance) => {
      const f = fixture({ reader });
      const live = await f.runtime.prepare(
        plan("a", [
          unit("source", "reader", [{ name: "files", contract: files.ref }]),
        ]),
      );
      const parentGrant = f.grant("a", live.objects, "parent");
      const parent = await f.runtime.accept(parentGrant);
      const childGrant = f.grant("a", live.objects, "child", parent.id);
      vi.mocked(f.control.acquire).mockResolvedValue(childGrant);
      const child = await (entrance === "accept"
        ? f.runtime.accept(childGrant)
        : f.runtime.acquire({
            operationId: childGrant.operationId,
            parentUseId: parent.id,
            execution: live.identity,
            snapshotId: "snapshot",
            selections: [],
          }));
      let release!: () => void;
      const work = child.with(live.objects[0]!, files, async (value) => {
        await new Promise<void>((resolve) => {
          release = resolve;
        });
        return value.read();
      });
      const closed = parent.close();
      const settled = vi.fn();
      void closed.then(settled);
      await Promise.resolve();
      expect(settled).not.toHaveBeenCalled();
      expect(child.lifetime.closing).toBe(true);
      expect(
        f.runtime.pendingProofs.some((proof) => proof.useId === parent.id),
      ).toBe(false);
      release();
      await work;
      expect((await closed).status).toBe("released");
      expect(() =>
        child.with(live.objects[0]!, files, (value) => value.read()),
      ).toThrow();
      await f.runtime.flushProofs();
      expect(
        [...f.proofs.values()].filter((proof) => proof.useId === parent.id),
      ).toHaveLength(1);
      expect(
        [...f.proofs.values()].filter((proof) => proof.useId === child.id),
      ).toHaveLength(1);
      await f.runtime.close();
    },
  );

  it("keeps an uncertain child reservation under its original parent", async () => {
    const f = fixture({ reader });
    const live = await f.runtime.prepare(
      plan("a", [
        unit("source", "reader", [{ name: "files", contract: files.ref }]),
      ]),
    );
    const parent = await f.runtime.accept(f.grant("a", live.objects, "parent"));
    const request = {
      operationId: "unknown-child",
      parentUseId: parent.id,
      execution: live.identity,
      snapshotId: "snapshot",
      selections: [],
    };
    vi.mocked(f.control.acquire).mockRejectedValue(
      new FrameError("outcome_unknown", "lost acknowledgement"),
    );
    await expect(f.runtime.acquire(request)).rejects.toMatchObject({
      code: "outcome_unknown",
    });
    expect(f.runtime.hasPendingAcquisition(request.operationId)).toBe(true);
    expect((await parent.close()).status).toBe("released");
    expect(f.runtime.hasPendingAcquisition(request.operationId)).toBe(false);
    await expect(f.runtime.acquire(request)).rejects.toMatchObject({
      code: "closed",
    });
    expect(f.control.acquire).toHaveBeenCalledOnce();
    await f.runtime.close();
  });

  it("shares an in-flight owner across acquire and accept", async () => {
    const f = fixture({ reader });
    const live = await f.runtime.prepare(
      plan("a", [
        unit("source", "reader", [{ name: "files", contract: files.ref }]),
      ]),
    );
    const grant = f.grant("a", live.objects, "original");
    let receive!: (value: UseGrant) => void;
    vi.mocked(f.control.acquire).mockImplementation(
      () =>
        new Promise((resolve) => {
          receive = resolve;
        }),
    );
    const acquired = f.runtime.acquire({
      operationId: grant.operationId,
      execution: live.identity,
      snapshotId: "snapshot",
      selections: [],
    });
    const accepted = f.runtime.accept(grant);
    const relabeled = expect(
      f.runtime.accept({ ...grant, id: "foreign-use" }),
    ).rejects.toMatchObject({ code: "permission_denied" });
    receive(grant);
    const [first, second] = await Promise.all([acquired, accepted]);
    await relabeled;
    expect(first).toBe(second);
    expect(f.control.validateUse).toHaveBeenCalledOnce();
    await first.close();
    await f.runtime.close();
    expect(
      [...f.proofs.values()].filter((proof) => proof.useId === grant.id),
    ).toHaveLength(1);
  });

  it("observes one terminal Use before proof ACK and rejects its historical grant afterward", async () => {
    const f = fixture({ reader });
    const live = await f.runtime.prepare(
      plan("a", [
        unit("source", "reader", [{ name: "files", contract: files.ref }]),
      ]),
    );
    const grant = f.grant("a", live.objects, "original");
    const request = {
      operationId: grant.operationId,
      execution: live.identity,
      snapshotId: "snapshot",
      selections: [],
    };
    vi.mocked(f.control.acquire).mockResolvedValue(grant);
    const original = await f.runtime.acquire(request);
    await original.close();
    vi.mocked(f.control.prove).mockRejectedValueOnce(
      new Error("lost proof acknowledgement"),
    );
    await expect(f.runtime.flushProofs()).rejects.toThrow(
      "lost proof acknowledgement",
    );
    expect(await f.runtime.acquire(request)).toBe(original);
    expect(await f.runtime.accept(grant)).toBe(original);
    expect(original.lifetime.closing).toBe(true);
    await f.runtime.flushProofs();
    await expect(f.runtime.acquire(request)).rejects.toMatchObject({
      code: "stale_binding",
    });
    await expect(f.runtime.accept(grant)).rejects.toMatchObject({
      code: "stale_binding",
    });
    expect(
      [...f.proofs.values()].filter((proof) => proof.useId === grant.id),
    ).toHaveLength(1);
    await f.runtime.close();
  });

  it("validates an acquisition receipt before attaching a request to a directly accepted Use", async () => {
    const f = fixture({ reader });
    const live = await f.runtime.prepare(
      plan("a", [
        unit("source", "reader", [{ name: "files", contract: files.ref }]),
      ]),
    );
    const grant = f.grant("a", live.objects, "original");
    const original = await f.runtime.accept(grant);
    const request = {
      operationId: grant.operationId,
      execution: live.identity,
      snapshotId: "snapshot",
      selections: [],
    };
    vi.mocked(f.control.acquire)
      .mockRejectedValueOnce(
        new FrameError(
          "invalid_plan",
          "operation has a different original input",
        ),
      )
      .mockResolvedValue(grant);
    await expect(
      f.runtime.acquire({ ...request, snapshotId: "foreign" }),
    ).rejects.toMatchObject({ code: "invalid_plan" });
    expect(original.lifetime.closing).toBe(false);
    expect(await f.runtime.acquire(request)).toBe(original);
    await expect(
      f.runtime.acquire({ ...request, snapshotId: "foreign" }),
    ).rejects.toMatchObject({ code: "invalid_plan" });
    expect(f.control.acquire).toHaveBeenCalledTimes(2);
    await f.runtime.close();
  });

  it("releases prepared resources after a Ready envelope is definitively refused", async () => {
    const f = fixture({ reader });
    vi.mocked(f.control.ready).mockRejectedValue(
      new FrameError("capacity", "ready envelope exceeds capacity"),
    );
    await expect(
      f.runtime.prepare(
        plan("a", [
          unit("source", "reader", [{ name: "files", contract: files.ref }]),
        ]),
      ),
    ).rejects.toMatchObject({ code: "capacity" });
    expect(f.runtime.pendingProofs).toMatchObject([
      { scopeKind: "execution", execution: { id: "a" }, status: "released" },
    ]);
    await f.runtime.close();
  });

  it("recovers a proof backlog larger than one transport message without aggregating evidence", async () => {
    const f = fixture({ reader });
    const live = await f.runtime.prepare(
      plan("a", [
        unit("source", "reader", [{ name: "files", contract: files.ref }]),
      ]),
    );
    for (let index = 0; index < 12; index++) {
      const use = await f.runtime.accept(f.grant("a", live.objects));
      await use.close();
    }
    const maximumMessageBytes = 1024;
    expect(
      new TextEncoder().encode(JSON.stringify(f.runtime.pendingProofs))
        .byteLength,
    ).toBeGreaterThan(maximumMessageBytes);
    const lifetime = new Lifetime("recovery-control");
    const delivered: CloseProof[] = [];
    const control = new HTTPControl({
      endpoint: "https://host.example/control",
      credential: "original-realm",
      maximumMessageBytes,
      lifetime,
      fetch: async (_url, options) => {
        const envelope = JSON.parse(String(options!.body));
        if (envelope.method === "reconnect")
          return Response.json({
            authority,
            realm,
            mode: "revalidated",
            executions: [
              { execution: live.identity, scopeId: "a", scopeGeneration: "1" },
            ],
          });
        expect(envelope.method).toBe("prove");
        const proof = envelope.input as CloseProof;
        delivered.push(proof);
        return Response.json({
          authority,
          closeId: proof.closeId,
          proofId: proof.proofId,
          recorded: true,
        });
      },
    });
    await f.runtime.reconnect(control);
    expect(delivered).toHaveLength(12);
    expect(f.runtime.pendingProofs).toHaveLength(0);
    await f.runtime.close();
    await lifetime.close();
  });

  it("routes native event payloads locally under one approved use", async () => {
    const event = new Event<{ object: object }>(
      ref("documents.changed", "event"),
    );
    const emitter = defineUnit({
      id: "events",
      imports: {},
      exports: { event },
      setup: () => ({ event: { object: {} } }),
    });
    const f = fixture({ events: emitter });
    const live = await f.runtime.prepare(
      plan("a", [
        unit("events", "events", [{ name: "event", contract: event.ref }]),
      ]),
    );
    const grant = f.grant("a", live.objects);
    const listener = vi.fn();
    const route = await f.runtime.route(
      {
        id: "route",
        use: grant,
        source: live.objects[0]!.export,
        receiver: live.objects[0]!.export,
      },
      event,
      listener,
    );
    const payload = { object: {} };
    await route.dispatch(payload);
    await route.dispatch(payload);
    expect(listener.mock.calls[0]![0]).toBe(payload);
    expect(f.control.validateRoute).toHaveBeenCalledOnce();
    expect(f.control.validateUse).toHaveBeenCalledOnce();
    await route.close();
    await expect(route.dispatch(payload)).rejects.toMatchObject({
      code: "closed",
    });
    await f.runtime.close();
  });

  it("materializes the route grant that crossed its validation boundary", async () => {
    const event = new Event<void>(ref("documents.changed", "event"));
    const f = fixture({
      events: defineUnit({
        id: "events",
        imports: {},
        exports: { event },
        setup: () => ({ event: undefined }),
      }),
    });
    const live = await f.runtime.prepare(
      plan("a", [
        unit("events", "events", [{ name: "event", contract: event.ref }]),
      ]),
    );
    const approved = f.grant("a", live.objects);
    const replacement = f.grant("a", live.objects);
    const received = {
      id: "route",
      use: approved,
      source: live.objects[0]!.export,
      receiver: live.objects[0]!.export,
    };
    const validation = Promise.withResolvers<void>();
    vi.mocked(f.control.validateRoute).mockReturnValueOnce(validation.promise);
    const pending = f.runtime.route(received, event, () => {});
    received.use = replacement;
    validation.resolve();
    const route = await pending;
    expect(f.control.validateUse).toHaveBeenCalledWith(approved);
    await route.close();
    await f.runtime.close();
  });

  it("retains failed consumers and their providers while independent work exits", async () => {
    let proof!: Cleanup;
    const released: string[] = [];
    const provider = defineUnit({
      id: "provider",
      imports: {},
      exports: { files },
      setup({ lifetime }) {
        lifetime.cleanup(() => {
          released.push("provider");
        });
        return { files: { read: () => "owned" } };
      },
    });
    const consumer = defineUnit({
      id: "consumer",
      imports: { source: files },
      exports: { preview },
      setup({ imports, lifetime }) {
        proof = lifetime.cleanup(() => {
          throw new Error("not released");
        });
        return { preview: { text: () => imports.source.read() } };
      },
    });
    const independent = defineUnit({
      id: "independent",
      imports: {},
      exports: {},
      setup({ lifetime }) {
        lifetime.cleanup(() => {
          released.push("independent");
        });
        return {};
      },
    });
    const f = fixture({ provider, consumer, independent });
    const live = await f.runtime.prepare(
      plan("a", [
        unit("provider", "provider", [{ name: "files", contract: files.ref }]),
        unit(
          "consumer",
          "consumer",
          [{ name: "preview", contract: preview.ref }],
          [
            {
              slot: "source",
              contract: files.ref,
              kind: "local",
              local: {
                instanceId: "provider",
                name: "files",
                contract: files.ref,
              },
            },
          ],
        ),
        unit("independent", "independent", []),
      ]),
    );
    expect((await live.close()).status).toBe("unconfirmed");
    expect(released).toEqual(["independent"]);
    expect(f.runtime.pendingProofs).toHaveLength(0);
    proof.confirmReleased("original resource confirmed termination");
    expect((await live.wait()).status).toBe("released");
    expect(released).toEqual(["independent", "provider"]);
    await f.runtime.flushProofs();
    expect(f.proofs.size).toBe(1);
  });

  it("replays immutable proof after lost ACK without rerunning cleanup or reloading early", async () => {
    let cleanup = 0;
    const owned = defineUnit({
      id: "owned",
      imports: {},
      exports: {},
      setup({ lifetime }) {
        lifetime.cleanup(() => {
          cleanup++;
        });
        return {};
      },
    });
    const f = fixture({ owned });
    await f.runtime.prepare(plan("a", [unit("owned", "owned", [])]));
    const original = f.control.prove;
    vi.mocked(f.control.prove).mockImplementationOnce(async (proof) => {
      await original(proof);
      throw new Error("ack lost");
    });
    const reload = vi.fn();
    await expect(f.runtime.reload(reload)).rejects.toThrow("ack lost");
    expect(reload).not.toHaveBeenCalled();
    expect(cleanup).toBe(1);
    const proof = f.runtime.pendingProofs[0]!;
    await f.runtime.reload(reload);
    expect(reload).toHaveBeenCalledOnce();
    expect(cleanup).toBe(1);
    expect(f.proofs.get(proof.proofId)).toEqual(proof);
  });

  it("invalidates concurrent old polls when a manual execution settles during their observation", async () => {
    const f = fixture({ reader }, 1);
    const original = plan("original", [
      unit("original-reader", "reader", [
        { name: "files", contract: files.ref },
      ]),
    ]);
    const firstReply = Promise.withResolvers<Poll>();
    const secondReply = Promise.withResolvers<Poll>();
    vi.mocked(f.control.poll)
      .mockReturnValueOnce(firstReply.promise)
      .mockReturnValueOnce(secondReply.promise);
    const firstSchedule = vi.fn<(plans: readonly LocalPlan[]) => void>();
    const secondSchedule = vi.fn<(plans: readonly LocalPlan[]) => void>();
    const firstPoll = f.runtime.poll(firstSchedule);
    const secondPoll = f.runtime.poll(secondSchedule);

    const execution = await f.runtime.prepare(original);
    await execution.close();
    await f.runtime.flushProofs();
    const next = await f.runtime.prepare(
      plan("next", [
        unit("next-reader", "reader", [{ name: "files", contract: files.ref }]),
      ]),
    );
    const oldReply: Poll = {
      authority,
      realm,
      plans: [original],
      fences: [],
    };
    firstReply.resolve(oldReply);
    await firstPoll;
    expect(firstSchedule).toHaveBeenCalledExactlyOnceWith([]);
    await next.close();
    await f.runtime.flushProofs();

    const fresh = plan("fresh", [
      unit("fresh-reader", "reader", [{ name: "files", contract: files.ref }]),
    ]);
    vi.mocked(f.control.poll).mockResolvedValueOnce({
      ...oldReply,
      plans: [fresh],
    });
    const freshSchedule = vi.fn<(plans: readonly LocalPlan[]) => void>();
    await f.runtime.poll(freshSchedule);
    expect(freshSchedule).toHaveBeenCalledExactlyOnceWith([fresh]);

    secondReply.resolve(oldReply);
    await secondPoll;
    expect(secondSchedule).toHaveBeenCalledExactlyOnceWith([]);
    expect(f.control.admitPlan).toHaveBeenCalledTimes(2);
    expect(f.runtime.pendingProofs).toEqual([]);
    await f.runtime.close();
  });

  it.each(["notAdmitted", "settled"] as const)(
    "invalidates an old poll when its original admission is %s",
    async (decision) => {
      const setup = vi.fn(() => ({}));
      const f = fixture(
        { owned: defineUnit({ id: "owned", imports: {}, exports: {}, setup }) },
        1,
      );
      const original = plan("original", [unit("original-unit", "owned", [])]);
      const reply = Promise.withResolvers<Poll>();
      vi.mocked(f.control.poll).mockReturnValueOnce(reply.promise);
      const schedule = vi.fn<(plans: readonly LocalPlan[]) => void>();
      const polling = f.runtime.poll(schedule);
      vi.mocked(f.control.admitPlan).mockResolvedValueOnce({
        authority,
        realm,
        execution: original.execution,
        decision,
      });
      await expect(f.runtime.prepare(original)).rejects.toMatchObject({
        code: "stale_binding",
      });
      expect(setup).not.toHaveBeenCalled();
      const next = await f.runtime.prepare(
        plan("next", [unit("next-unit", "owned", [])]),
      );
      reply.resolve({
        authority,
        realm,
        plans: [original],
        fences: [],
      });
      await polling;
      expect(schedule).toHaveBeenCalledExactlyOnceWith([]);
      expect(f.control.admitPlan).toHaveBeenCalledTimes(2);
      expect(f.control.prove).not.toHaveBeenCalled();
      await next.close();
      await f.runtime.close();
    },
  );

  it("still applies fences when an acknowledgement invalidates older pending plans", async () => {
    const f = fixture({ reader }, 2);
    const releasedPlan = plan("released", [
      unit("released-reader", "reader", [
        { name: "files", contract: files.ref },
      ]),
    ]);
    const activePlan = plan("active", [
      unit("active-reader", "reader", [{ name: "files", contract: files.ref }]),
    ]);
    const released = await f.runtime.prepare(releasedPlan);
    const active = await f.runtime.prepare(activePlan);
    const reply = Promise.withResolvers<Poll>();
    vi.mocked(f.control.poll).mockReturnValueOnce(reply.promise);
    const schedule = vi.fn<(plans: readonly LocalPlan[]) => void>();
    const polling = f.runtime.poll(schedule);
    await released.close();
    await f.runtime.flushProofs();
    reply.resolve({
      authority,
      realm,
      plans: [releasedPlan],
      fences: [
        {
          scopeId: activePlan.scopeId,
          scopeGeneration: activePlan.scopeGeneration,
        },
      ],
    });
    await polling;
    expect(schedule).toHaveBeenCalledExactlyOnceWith([]);
    expect((await active.wait()).status).toBe("released");
    expect(f.runtime.pendingProofs.map((proof) => proof.execution.id)).toEqual([
      "active",
    ]);
    await f.runtime.close();
  });

  it("revalidates only live executions after a released execution loses its proof acknowledgement", async () => {
    const f = fixture({ reader });
    const released = await f.runtime.prepare(
      plan("released", [
        unit("released-reader", "reader", [
          { name: "files", contract: files.ref },
        ]),
      ]),
    );
    const active = await f.runtime.prepare(
      plan("active", [
        unit("active-reader", "reader", [
          { name: "files", contract: files.ref },
        ]),
      ]),
    );
    await released.close();
    const prove = vi.mocked(f.control.prove).getMockImplementation()!;
    vi.mocked(f.control.prove).mockImplementationOnce(async (proof) => {
      await prove(proof);
      throw new FrameError("outcome_unknown", "proof acknowledgement was lost");
    });
    await expect(f.runtime.flushProofs()).rejects.toMatchObject({
      code: "outcome_unknown",
    });
    const originalProof = f.runtime.pendingProofs[0]!;
    f.runtime.disconnect();
    await f.runtime.reconnect(f.control);
    expect(f.control.reconnect).toHaveBeenCalledExactlyOnceWith({
      authority,
      realm,
      executions: [active.identity],
    });
    expect(f.runtime.connection).toBe("connected");
    expect(f.runtime.pendingProofs).toEqual([]);
    expect(f.proofs.get(originalProof.proofId)).toEqual(originalProof);
    expect(active.objects).toHaveLength(1);
    await f.runtime.close();
  });

  it("keeps a dynamic native object after publication ACK loss and rejects disconnected admission", async () => {
    const contributor = defineUnit({
      id: "contributor",
      imports: {},
      exports: { additions },
      setup() {
        const value = { value: "base" };
        return { additions: contribution({ title: "base" }, value) };
      },
    });
    const f = fixture({ contributor });
    await f.runtime.prepare(
      plan("a", [
        unit("contributor", "contributor", [
          { name: "additions", contract: additions.ref },
        ]),
      ]),
    );
    vi.mocked(f.control.publish).mockImplementationOnce(
      async (request: PublicationRequest) => {
        f.publications.set(request.operationId, {
          decision: "accepted",
          publication: {
            id: "published",
            operationId: request.operationId,
            revision: "2",
            object: request.object,
          },
        });
        throw new Error("ACK lost");
      },
    );
    const publication = await f.runtime.publish(
      { id: "a", generation: "1" },
      "contributor",
      additions,
      "dynamic",
      { title: "Original" },
      { value: "native object" },
    );
    expect(publication.outcome.decision).toBe("unknown");
    const use = await f.runtime.accept(f.grant("a", [publication.object]));
    expect(
      await use.with(publication.object, additions, (value) => value.value),
    ).toBe("native object");
    f.runtime.disconnect();
    await expect(
      f.runtime.accept(f.grant("a", [publication.object])),
    ).rejects.toThrow("no new framework work");
    await f.runtime.reconnect(f.control);
    expect((await publication.observe()).decision).toBe("accepted");
    expect((await publication.withdraw()).decision).toBe("accepted");
    expect(
      f.runtime.pendingProofs.some(
        (proof) => proof.scopeKind === "publication",
      ),
    ).toBe(false);
    expect(
      await use.with(publication.object, additions, (value) => value.value),
    ).toBe("native object");
    await use.close();
    await vi.waitFor(() =>
      expect(
        f.runtime.pendingProofs.some(
          (proof) => proof.publicationId === "published",
        ),
      ).toBe(true),
    );
    expect(
      f.runtime.pendingProofs.find(
        (proof) => proof.publicationId === "published",
      )?.instances,
    ).toEqual(["contributor"]);
    await expect(
      f.runtime.accept(f.grant("a", [publication.object])),
    ).rejects.toMatchObject({ code: "stale_binding" });
    await expect(publication.withdraw()).rejects.toMatchObject({
      code: "closed",
    });
    await f.runtime.close();
  });

  it("stages setup contributions in ready without waiting for its own publication ACK", async () => {
    const contributor = defineUnit({
      id: "contributor",
      imports: {},
      exports: { additions },
      setup() {
        const metadata = { title: "before" };
        return { additions: contribution(metadata, { value: "kept" }) };
      },
    });
    const f = fixture({ contributor });
    await f.runtime.prepare(
      plan("a", [
        unit("contributor", "contributor", [
          { name: "additions", contract: additions.ref },
        ]),
      ]),
    );
    expect(f.ready.get("a")!.contributions[0]!.metadataJson).toBe(
      '{"title":"before"}',
    );
    expect(f.control.publish).not.toHaveBeenCalled();
    await f.runtime.close();
  });

  it("covers accepted dynamic objects with their execution exit without a separate withdrawal", async () => {
    const cleanup = vi.fn();
    const contributor = defineUnit({
      id: "contributor",
      imports: {},
      exports: { additions },
      setup(context) {
        context.lifetime.cleanup(cleanup);
        const value = { value: "base" };
        return { additions: contribution({ title: "base" }, value) };
      },
    });
    const f = fixture({ contributor });
    const live = await f.runtime.prepare(
      plan("a", [
        unit("contributor", "contributor", [
          { name: "additions", contract: additions.ref },
        ]),
      ]),
    );
    const publication = await f.runtime.publish(
      live.identity,
      "contributor",
      additions,
      "dynamic",
      { title: "Dynamic" },
      { value: "owned" },
    );
    expect(publication.outcome.decision).toBe("accepted");
    expect((await f.runtime.close())[0]!.status).toBe("released");
    expect(cleanup).toHaveBeenCalledOnce();
    expect(f.control.withdraw).not.toHaveBeenCalled();
    expect([...f.proofs.values()].map((proof) => proof.scopeKind)).toEqual([
      "execution",
    ]);
    expect(f.runtime.pendingProofs).toHaveLength(0);
    await f.runtime.reload(() => {});
    expect(cleanup).toHaveBeenCalledOnce();
  });

  it("keeps execution protection when dynamic objects exit before another cleanup is confirmed", async () => {
    let cleanup!: Cleanup;
    const contributor = defineUnit({
      id: "contributor",
      imports: {},
      exports: { additions },
      setup(context) {
        cleanup = context.lifetime.cleanup(() => ({ status: "unconfirmed" }));
        const value = { value: "base" };
        return { additions: contribution({ title: "base" }, value) };
      },
    });
    const f = fixture({ contributor });
    const live = await f.runtime.prepare(
      plan("a", [
        unit("contributor", "contributor", [
          { name: "additions", contract: additions.ref },
        ]),
      ]),
    );
    const publication = await f.runtime.publish(
      live.identity,
      "contributor",
      additions,
      "dynamic",
      { title: "Dynamic" },
      { value: "owned" },
    );
    expect(publication.outcome.decision).toBe("accepted");
    expect((await f.runtime.close())[0]!.status).toBe("unconfirmed");
    expect(f.control.prove).not.toHaveBeenCalled();
    expect(f.runtime.pendingProofs).toHaveLength(0);
    const reload = vi.fn();
    await expect(f.runtime.reload(reload)).rejects.toMatchObject({
      code: "close_unconfirmed",
    });
    expect(reload).not.toHaveBeenCalled();
    cleanup.confirmReleased("original resource termination confirmed");
    expect((await live.wait()).status).toBe("released");
    await f.runtime.reload(reload);
    expect(reload).toHaveBeenCalledOnce();
    expect([...f.proofs.values()].map((proof) => proof.scopeKind)).toEqual([
      "execution",
    ]);
  });

  it("retains terminal handles without retaining their Runtime or native object payloads", async () => {
    const collect = (globalThis as { gc?: () => void }).gc;
    if (!collect)
      throw new Error("this required retention test needs Node --expose-gc");
    const { retained, references } = await closedHandles();
    let remaining: string[] = [];
    for (let attempt = 0; attempt < 50; attempt++) {
      await new Promise((resolve) => setTimeout(resolve, 0));
      collect();
      await new Promise((resolve) => setTimeout(resolve, 0));
      remaining = references
        .filter((entry) => entry.reference.deref() !== undefined)
        .map((entry) => entry.name);
      if (!remaining.length) break;
    }
    expect(remaining).toEqual([]);
    expect(retained.execution.identity.id).toBe("retention");
    expect(retained.use.id).toBeTruthy();
    expect(retained.exit.outcome.status).toBe("released");
    expect(retained.publication.outcome.decision).toBe("accepted");
    await expect(retained.route.dispatch({ object: {} })).rejects.toMatchObject(
      {
        code: "closed",
      },
    );
  });
});

async function closedHandles() {
  const payload = { bytes: new Uint8Array(8 * 1024 * 1024) };
  const dynamic = { bytes: new Uint8Array(8 * 1024 * 1024) };
  const point = new Point<{ title: string }, typeof payload>(
    ref("documents.retention", "point"),
  );
  const event = new Event<{ object: object }>(
    ref("documents.retention.changed", "event"),
  );
  const definition = defineUnit({
    id: "retention",
    imports: {},
    exports: { payload: point, changes: event },
    setup() {
      return {
        payload: contribution({ title: "original" }, payload),
        changes: { object: payload },
      };
    },
  });
  const unavailable = async () => {
    throw new Error("unexpected retention fixture control operation");
  };
  // Instrumented mocks keep their implementations in Vitest's global registry.
  // This fixture uses ordinary functions so only the public handles retain state.
  const control: HostControl = {
    admitPlan: async (plan) => ({
      authority,
      realm,
      execution: plan.execution,
      decision: "admitted",
    }),
    ready: async () => ({ decision: "accepted" }),
    acquire: unavailable,
    validateUse: async () => {},
    validateRoute: async () => {},
    snapshot: unavailable,
    closeSnapshot: unavailable,
    publish: async (request) => ({
      decision: "accepted",
      publication: {
        id: "retained-publication",
        operationId: request.operationId,
        revision: "1",
        object: request.object,
      },
    }),
    publication: unavailable,
    withdraw: unavailable,
    prove: async (proof) => ({
      proofId: proof.proofId,
      closeId: proof.closeId,
      authority,
      recorded: true,
    }),
    reconnect: unavailable,
    poll: unavailable,
  };
  const runtime = new Runtime({
    authority,
    realm,
    control,
    proofCapacity: 32,
    load: () => definition,
    remote: unavailable,
  });
  const execution = await runtime.prepare(
    plan("retention", [
      unit("retention", "retention", [
        { name: "payload", contract: point.ref },
        { name: "changes", contract: event.ref },
      ]),
    ]),
  );
  const publication = await runtime.publish(
    execution.identity,
    "retention",
    point,
    "dynamic",
    { title: "dynamic" },
    dynamic,
  );
  const grant: UseGrant = {
    operationId: "retention-use",
    id: "retention-use",
    approvalToken: "approved-retention-use",
    authority,
    realm,
    execution: execution.identity,
    scopeId: "retention",
    scopeGeneration: "1",
    objects: execution.objects,
  };
  const use = await runtime.accept(grant);
  const eventObject = execution.objects.find(
    (object) => object.export.name === "changes",
  )!;
  const route = await runtime.route(
    {
      id: "retention-route",
      use: grant,
      source: eventObject.export,
      receiver: eventObject.export,
    },
    event,
    () => {},
  );
  const exit = use.lifetime.beginClose();
  await runtime.close();
  expect(runtime.pendingProofs).toHaveLength(0);
  return {
    retained: { execution, use, exit, publication, route },
    references: [
      { name: "Runtime", reference: new WeakRef(runtime) },
      { name: "static object", reference: new WeakRef(payload) },
      { name: "dynamic object", reference: new WeakRef(dynamic) },
    ],
  };
}

it.each(["disconnect", "reconnect", "fence"])(
  "rechecks authority after slow proxy creation on %s",
  async (mode) => {
    const service = new Service({ ...files.ref, mode: "portable" });
    const setup = vi.fn(() => ({}));
    const definition = defineUnit({
      id: "sink",
      imports: { value: service },
      exports: {},
      setup,
    });
    const f = fixture({});
    const entered = Promise.withResolvers<void>(),
      proceed = Promise.withResolvers<void>();
    const runtime = new Runtime({
      authority,
      realm,
      control: f.control,
      proofCapacity: 2,
      load: () => definition,
      remote: async () => {
        entered.resolve();
        await proceed.promise;
        return {};
      },
    });
    const item = unit(
      "u",
      "sink",
      [],
      [
        {
          slot: "value",
          kind: "remote",
          contract: service.ref,
          remote: {
            token: "approved",
            source: {
              installationId: "src",
              release: digest,
              unitId: "source",
              export: "service",
              contract: service.ref,
            },
          },
        },
      ],
    );
    const preparing = runtime.prepare(plan("scope", [item]));
    await entered.promise;
    if (mode === "fence") runtime.fence("scope", "1");
    else {
      runtime.disconnect();
      if (mode === "reconnect") await runtime.reconnect(f.control);
    }
    proceed.resolve();
    await expect(preparing).rejects.toThrow();
    expect(setup).not.toHaveBeenCalled();
    expect(f.control.ready).not.toHaveBeenCalled();
    await runtime.close();
  },
);

it("represents optional prototype-named slots as own absent properties", async () => {
  const names = ["__proto__", "constructor", "toString"];
  const f = fixture({
    sink: defineUnit({
      id: "sink",
      imports: Object.fromEntries(names.map((name) => [name, optional(files)])),
      exports: {},
      setup({ imports }) {
        for (const name of names) {
          expect(Object.hasOwn(imports, name)).toBe(true);
          expect(imports[name]).toBeUndefined();
        }
        return {};
      },
    }),
  });
  await f.runtime.prepare(
    plan("scope", [
      unit(
        "u",
        "sink",
        [],
        names.map((slot) => ({ slot, contract: files.ref, kind: "absent" })),
      ),
    ]),
  );
  await f.runtime.close();
});

it("returns each confirmed refusal's reservation without closing the execution", async () => {
  const f = fixture({});
  const definition = defineUnit({
    id: "sink",
    imports: {},
    exports: { point: additions },
    setup: () => ({
      point: contribution({ title: "base" }, { value: "base" }),
    }),
  });
  const runtime = new Runtime({
    authority,
    realm,
    control: f.control,
    proofCapacity: 2,
    load: () => definition,
    remote: () => {},
  });
  const { identity } = await runtime.prepare(
    plan("scope", [
      unit("u", "sink", [{ name: "point", contract: additions.ref }]),
    ]),
  );
  const refusal = new FrameError("capacity", "confirmed refusal");
  vi.mocked(f.control.acquire).mockRejectedValue(refusal);
  vi.mocked(f.control.publish).mockRejectedValue(refusal);
  for (let index = 0; index < 12; index++) {
    await expect(
      runtime.acquire({
        operationId: String(index),
        execution: identity,
        snapshotId: "expired",
        selections: [],
      }),
    ).rejects.toBe(refusal);
    expect(runtime.hasPendingAcquisition(String(index))).toBe(false);
    await expect(
      runtime.publish(
        identity,
        "u",
        additions,
        "item",
        { title: "x" },
        { value: "x" },
      ),
    ).rejects.toBe(refusal);
  }
  vi.mocked(f.control.publish).mockResolvedValue({ decision: "rejected" });
  expect(
    (
      await runtime.publish(
        identity,
        "u",
        additions,
        "item",
        { title: "x" },
        { value: "x" },
      )
    ).outcome.decision,
  ).toBe("rejected");
  vi.mocked(f.control.publish).mockResolvedValue({ decision: "accepted" });
  const unknown = await runtime.publish(
    identity,
    "u",
    additions,
    "item",
    { title: "x" },
    { value: "x" },
  );
  expect(unknown.outcome).toMatchObject({
    decision: "unknown",
    problem: expect.any(String),
  });
  vi.mocked(f.control.publication).mockResolvedValue({ decision: "rejected" });
  expect((await unknown.observe()).decision).toBe("rejected");
  await runtime.close();
});

it.each([null, undefined])(
  "rejects a missing Point implementation at every staging entrance: %s",
  async (missing) => {
    const makeUnit = (value: { value: string }) =>
      defineUnit({
        id: "contributor",
        imports: {},
        exports: { additions },
        setup: () => ({ additions: contribution({ title: "base" }, value) }),
      });
    const approved = plan("a", [
      unit("contributor", "contributor", [
        { name: "additions", contract: additions.ref },
      ]),
    ]);
    const incomplete = fixture({
      contributor: makeUnit(missing as unknown as { value: string }),
    });
    await expect(incomplete.runtime.prepare(approved)).rejects.toMatchObject({
      code: "contract_mismatch",
    });
    expect(incomplete.control.ready).not.toHaveBeenCalled();
    await incomplete.runtime.close();

    const f = fixture({ contributor: makeUnit({ value: "base" }) }, 2);
    await f.runtime.prepare(approved);
    await expect(
      f.runtime.publish(
        approved.execution,
        "contributor",
        additions,
        "dynamic",
        { title: "dynamic" },
        missing as unknown as { value: string },
      ),
    ).rejects.toMatchObject({ code: "contract_mismatch" });
    expect(f.control.publish).not.toHaveBeenCalled();
    const publication = await f.runtime.publish(
      approved.execution,
      "contributor",
      additions,
      "dynamic",
      { title: "dynamic" },
      { value: "present" },
    );
    expect(publication.outcome.decision).toBe("accepted");
    await f.runtime.close();
  },
);

it("retains publication identity and retries the same withdrawal after refusal or uncertainty", async () => {
  const contributor = defineUnit({
    id: "contributor",
    imports: {},
    exports: { additions },
    setup: () => ({
      additions: contribution({ title: "base" }, { value: "base" }),
    }),
  });
  const f = fixture({ contributor });
  const approved = plan("a", [
    unit("contributor", "contributor", [
      { name: "additions", contract: additions.ref },
    ]),
  ]);
  await f.runtime.prepare(approved);
  const publication = await f.runtime.publish(
    approved.execution,
    "contributor",
    additions,
    "dynamic",
    { title: "dynamic" },
    { value: "present" },
  );
  const original = publication.outcome;
  for (const changed of [{ id: "replacement" }, { revision: "next" }]) {
    vi.mocked(f.control.publication).mockResolvedValue({
      ...original,
      publication: { ...original.publication!, ...changed },
    });
    await expect(publication.observe()).rejects.toMatchObject({
      code: "permission_denied",
    });
    expect(publication.outcome).toBe(original);
  }
  vi.mocked(f.control.publication).mockResolvedValue({ decision: "unknown" });
  expect(await publication.observe()).toBe(original);
  vi.mocked(f.control.withdraw)
    .mockResolvedValueOnce({ decision: "rejected" })
    .mockResolvedValueOnce({ decision: "unknown" });
  expect((await publication.withdraw()).decision).toBe("rejected");
  expect((await publication.withdraw()).decision).toBe("unknown");
  expect((await publication.withdraw()).decision).toBe("accepted");
  const calls = vi.mocked(f.control.withdraw).mock.calls;
  expect(calls[0]![0]).toEqual(calls[1]![0]);
  expect(calls[0]![0]).toEqual(calls[2]![0]);
  expect(calls[0]![0]).toEqual({
    authority,
    publicationId: original.publication!.id,
  });
  expect(
    f.runtime.pendingProofs.find((proof) => proof.scopeKind === "publication")
      ?.publicationId,
  ).toBe(original.publication!.id);
  await f.runtime.close();
});

it.each(["accepted", "unknown"] as const)(
  "preserves a rejected publication after an older observation returns %s",
  async (lateDecision) => {
    const contributor = defineUnit({
      id: "contributor",
      imports: {},
      exports: { additions },
      setup: () => ({
        additions: contribution({ title: "base" }, { value: "base" }),
      }),
    });
    const f = fixture({ contributor });
    const approved = plan("a", [
      unit("contributor", "contributor", [
        { name: "additions", contract: additions.ref },
      ]),
    ]);
    await f.runtime.prepare(approved);
    vi.mocked(f.control.publish).mockResolvedValueOnce({ decision: "unknown" });
    const publication = await f.runtime.publish(
      approved.execution,
      "contributor",
      additions,
      "dynamic",
      { title: "dynamic" },
      { value: "present" },
    );
    const lateReply = Promise.withResolvers<PublicationResult>();
    vi.mocked(f.control.publication)
      .mockReturnValueOnce(lateReply.promise)
      .mockResolvedValueOnce({ decision: "rejected" });
    const earlierObservation = publication.observe();
    const rejected = await publication.observe();
    expect(rejected.decision).toBe("rejected");
    lateReply.resolve(
      lateDecision === "unknown"
        ? { decision: "unknown" }
        : {
            decision: "accepted",
            publication: {
              id: "unexpected-publication",
              operationId: publication.operationId,
              revision: "1",
              object: publication.object,
            },
          },
    );
    if (lateDecision === "accepted")
      await expect(earlierObservation).rejects.toMatchObject({
        code: "invalid_plan",
      });
    else expect(await earlierObservation).toBe(rejected);
    expect(publication.outcome).toBe(rejected);
    expect(f.control.withdraw).not.toHaveBeenCalled();
    await f.runtime.close();
  },
);
