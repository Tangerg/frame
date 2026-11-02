import { describe, expect, it, vi } from "vitest";
import {
  contribution,
  FrameError,
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
  type PlannedUnit,
  type PublicationRequest,
  type PublicationResult,
  type ReadyReport,
  type UseGrant,
} from "../protocol.generated.js";
import { Runtime } from "./runtime";
import type { HostControl } from "./control";

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

function fixture(definitions: Record<string, Unit>) {
  const ready = new Map<string, ReadyReport>();
  const grants = new Map<string, UseGrant>();
  const publications = new Map<string, PublicationResult>();
  const proofs = new Map<string, CloseProof>();
  let nextGrant = 0;
  const control: HostControl = {
    validatePlan: vi.fn<HostControl["validatePlan"]>(async (value) => {
      if (value.approvalToken !== `plan:${value.execution.id}`)
        throw new Error("invalid plan approval");
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
        throw new Error("invalid use approval");
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
      revision: "1",
      plans: [],
      fences: [],
    })),
    reconnect: vi.fn<HostControl["reconnect"]>(async (request) => ({
      authority,
      realm,
      mode: "revalidated",
      acceptedProofs: [],
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
    proofCapacity: 128,
    load: (entry) => definitions[entry.module.path]!,
    remote: () => {
      throw new Error("unexpected remote import");
    },
  });
  function grant(id: string, objects = ready.get(id)!.objects): UseGrant {
    const value: UseGrant = {
      id: `use:${++nextGrant}`,
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
    vi.mocked(f.control.ready).mockImplementationOnce(async (report) => {
      f.ready.set(report.execution.id, report);
      throw new Error("ACK lost");
    });
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
    await f.runtime.prepare(
      plan("a", [
        unit("source", "reader", [{ name: "files", contract: files.ref }]),
      ]),
    );
    expect(f.ready.get("a")).toEqual(original);
    expect(f.control.ready).toHaveBeenCalledTimes(2);
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
    const grant = f.grant("a", live.objects);
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
    const grant = f.grant("a", live.objects);
    vi.mocked(f.control.acquire)
      .mockRejectedValueOnce(new Error("lost reply"))
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
    const use = await f.runtime.acquire(request);
    expect(f.runtime.hasPendingAcquisition(request.operationId)).toBe(false);
    expect(await f.runtime.acquire(request)).toBe(use);
    expect(f.control.acquire).toHaveBeenCalledTimes(2);
    expect(
      await use.with(live.objects[0]!, files, (value) => value.read()),
    ).toBe("source");
    await f.runtime.reload(() => {});
    expect(
      [...f.proofs.values()].filter((proof) => proof.useId === grant.id),
    ).toHaveLength(1);
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
    validatePlan: async () => {},
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
