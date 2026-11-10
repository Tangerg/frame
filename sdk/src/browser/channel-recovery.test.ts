import { createServer } from "node:http";
import { describe, expect, it, onTestFinished, vi } from "vitest";
import { defineUnit, Event, Lifetime, Service } from "../index.js";
import {
  PROTOCOL,
  type AcquireRequest,
  type CloseProof,
  type ControlInputs,
  type LocalPlan,
  type ReadyReport,
  type RouteGrant,
  type UseGrant,
} from "../protocol.generated.js";
import { HTTPControl } from "./http.js";
import { ControlPump } from "./pump.js";
import { Runtime } from "./runtime.js";

const authority = { id: "host", epoch: "1" };
const realm = { id: "realm", epoch: "1" };
const execution = { id: "execution", generation: "1" };
const digest = "a".repeat(64);
const files = new Service<{ read(): string }>({
  name: "files",
  version: "1",
  digest,
  kind: "service",
  mode: "native",
});
const updates = new Event<string>({
  name: "updates",
  version: "1",
  digest,
  kind: "event",
  mode: "native",
});
const plan: LocalPlan = {
  protocol: PROTOCOL,
  authority,
  realm,
  execution,
  approvalToken: "original-plan",
  scopeId: "scope",
  scopeGeneration: "1",
  units: [
    {
      instance: {
        id: execution.id,
        generation: execution.generation,
        source: {
          installationId: "installation",
          release: digest,
          unitId: "unit",
        },
        configuration: digest,
      },
      configurationJson: "{}",
      profile: "browser-js",
      module: { path: "unit", digest },
      imports: [],
      exports: [
        { name: "files", contract: files.ref },
        { name: "updates", contract: updates.ref },
      ],
    },
  ],
};
const acquisition: AcquireRequest = {
  operationId: "original-acquisition",
  execution,
  snapshotId: "original-snapshot",
  selections: [],
};
type SuspendedMethod =
  | "poll"
  | "admitPlan"
  | "ready"
  | "acquire"
  | "validateUse"
  | "validateRoute"
  | "prove"
  | "reconnect";
interface Dispatch {
  readonly method: keyof ControlInputs;
  readonly input: unknown;
  readonly signal: AbortSignal;
  readonly path: string;
}

async function fixture(suspended: SuspendedMethod) {
  const entered = Promise.withResolvers<void>();
  const disconnected = Promise.withResolvers<void>();
  const dispatches: Dispatch[] = [];
  const reports: ReadyReport[] = [];
  const proofs: CloseProof[] = [];
  let held = false;
  const grant = (request = acquisition): UseGrant => ({
    id: "use:" + request.operationId,
    operationId: request.operationId,
    approvalToken: "original-use",
    authority,
    realm,
    execution,
    scopeId: plan.scopeId,
    scopeGeneration: plan.scopeGeneration,
    objects: reports[0]!.objects,
  });
  const server = createServer(async (request, response) => {
    const chunks: Buffer[] = [];
    for await (const chunk of request) chunks.push(Buffer.from(chunk));
    const { method, input } = JSON.parse(Buffer.concat(chunks).toString()) as {
      method: keyof ControlInputs;
      input: unknown;
    };
    if (method === "ready") reports.push(input as ReadyReport);
    if (method === "prove") proofs.push(input as CloseProof);
    if (request.url === "/retired" && method === suspended && !held) {
      held = true;
      response.on("close", disconnected.resolve);
      entered.resolve();
      return;
    }
    let result: unknown;
    switch (method) {
      case "admitPlan":
        result = { authority, realm, execution, decision: "admitted" };
        break;
      case "ready":
        result = { decision: "accepted" };
        break;
      case "acquire":
        result = grant(input as AcquireRequest);
        break;
      case "validateUse":
      case "validateRoute":
        result = null;
        break;
      case "poll":
        result = { authority, realm, plans: [], fences: [] };
        break;
      case "reconnect":
        result = {
          authority,
          realm,
          mode: "revalidated",
          executions: (input as ControlInputs["reconnect"]).executions.map(
            (execution) => ({
              execution,
              scopeId: plan.scopeId,
              scopeGeneration: plan.scopeGeneration,
            }),
          ),
        };
        break;
      case "prove": {
        const proof = input as CloseProof;
        result = {
          authority,
          closeId: proof.closeId,
          proofId: proof.proofId,
          recorded: true,
        };
        break;
      }
      default:
        response.statusCode = 500;
        result = {
          code: "unavailable",
          message: "unexpected control operation",
        };
    }
    response.setHeader("Content-Type", "application/json");
    response.end(JSON.stringify(result));
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  if (!address || typeof address === "string")
    throw new Error("missing test listener");
  const transports = [
    new Lifetime("retired transport"),
    new Lifetime("replacement transport"),
  ];
  const controls = transports.map(
    (lifetime, index) =>
      new HTTPControl({
        endpoint:
          "http://127.0.0.1:" +
          address.port +
          (index === 0 ? "/retired" : "/replacement"),
        credential: "test credential",
        maximumMessageBytes: 65536,
        lifetime,
        fetch(input, init) {
          const envelope = JSON.parse(init!.body as string) as {
            method: keyof ControlInputs;
            input: unknown;
          };
          dispatches.push({
            ...envelope,
            path: new URL(String(input)).pathname,
            signal: init!.signal!,
          });
          return fetch(input, init);
        },
      }),
  );
  const released = vi.fn();
  const setup = vi.fn(({ lifetime }: { lifetime: Lifetime }) => {
    lifetime.cleanup(released);
    return { files: { read: () => "original object" }, updates: "updates" };
  });
  const definition = defineUnit({
    id: "unit",
    imports: {},
    exports: { files, updates },
    setup,
  });
  const runtime = new Runtime({
    authority,
    realm,
    control: controls[0]!,
    proofCapacity: 8,
    load: () => definition,
    remote: () => {
      throw new Error("unexpected remote binding");
    },
  });
  const pump = new ControlPump(runtime, {
    intervalMilliseconds: 1,
    preparationCapacity: 1,
    onError: () => {},
  });
  onTestFinished(async () => {
    await Promise.all(transports.map((lifetime) => lifetime.close()));
    server.closeAllConnections();
    await new Promise<void>((resolve, reject) =>
      server.close((error) => (error ? reject(error) : resolve())),
    );
  });

  async function begin() {
    let start: () => Promise<unknown>;
    if (suspended === "poll") start = () => pump.synchronize();
    else if (suspended === "admitPlan" || suspended === "ready")
      start = () => runtime.prepare(plan);
    else if (suspended === "reconnect")
      start = () => runtime.reconnect(controls[0]!);
    else {
      const prepared = await runtime.prepare(plan);
      if (suspended === "prove") {
        await prepared.close();
        start = () => runtime.flushProofs();
      } else if (suspended === "validateRoute") {
        const reference = prepared.objects.find(
          (object) => object.export.name === "updates",
        )!;
        const route: RouteGrant = {
          id: "original-route",
          use: grant(),
          source: reference.export,
          receiver: reference.export,
        };
        start = () => runtime.route(route, updates, () => {});
      } else start = () => runtime.acquire(acquisition);
    }
    const original = start().then(
      (value) => ({ status: "fulfilled" as const, value }),
      (error: unknown) => ({ status: "rejected" as const, error }),
    );
    await entered.promise;
    return { original, resume: start };
  }

  return {
    controls,
    runtime,
    pump,
    begin,
    disconnected,
    dispatches,
    setup,
    released,
    reports,
    proofs,
  };
}

describe("authenticated channel attempt lifecycle", () => {
  it.each<SuspendedMethod>([
    "poll",
    "admitPlan",
    "ready",
    "acquire",
    "validateUse",
    "validateRoute",
    "prove",
    "reconnect",
  ])(
    "replaces a real HTTP %s request before the retired server replies",
    async (method) => {
      const f = await fixture(method);
      const { original, resume } = await f.begin();
      await f.runtime.reconnect(f.controls[1]!);
      expect(f.runtime.control).toBe(f.controls[1]);
      expect(f.runtime.connection).toBe("connected");
      await f.disconnected.promise;
      expect(
        f.dispatches.find(
          (entry) => entry.path === "/retired" && entry.method === method,
        )!.signal.aborted,
      ).toBe(true);

      const first = await original;
      if (["poll", "prove"].includes(method))
        expect(first.status).toBe("fulfilled");
      else
        expect(first).toMatchObject({
          status: "rejected",
          error: {
            code: method === "reconnect" ? "unavailable" : "outcome_unknown",
          },
        });
      if (method !== "reconnect") await resume();
      expect(f.runtime.control).toBe(f.controls[1]);
      if (
        [
          "admitPlan",
          "ready",
          "acquire",
          "validateUse",
          "validateRoute",
        ].includes(method)
      )
        expect(f.setup).toHaveBeenCalledOnce();
      if (method === "admitPlan")
        expect(
          f.dispatches
            .filter((entry) => entry.method === "admitPlan")
            .map((entry) => entry.input),
        ).toEqual([plan, plan]);
      if (method === "ready")
        expect(f.reports).toEqual([f.reports[0], f.reports[0]]);
      if (method === "acquire" || method === "validateUse")
        expect(
          f.dispatches
            .filter((entry) => entry.method === "acquire")
            .map((entry) => entry.input),
        ).toEqual([acquisition, acquisition]);
      if (method === "prove")
        expect(f.proofs).toEqual([f.proofs[0], f.proofs[0]]);
      expect((await f.controls[0]!.close()).status).toBe("released");
      await f.pump.close();
      expect((await f.controls[1]!.close()).status).toBe("released");
    },
  );

  it.each<SuspendedMethod>([
    "poll",
    "admitPlan",
    "ready",
    "acquire",
    "validateUse",
    "validateRoute",
    "prove",
    "reconnect",
  ])(
    "closes through a real suspended HTTP %s request without losing its original obligation",
    async (method) => {
      const f = await fixture(method);
      const { original } = await f.begin();
      await f.pump.close();
      await original;
      await f.disconnected.promise;
      expect(f.runtime.connection).toBe("closed");
      expect(f.runtime.pendingProofs).toEqual([]);
      expect((await f.controls[0]!.close()).status).toBe("released");
      const uses = f.proofs.filter((proof) => proof.scopeKind === "use");
      expect(uses).toEqual([]);
      if (["poll", "admitPlan", "reconnect"].includes(method))
        expect(f.setup).not.toHaveBeenCalled();
      else {
        expect(f.setup).toHaveBeenCalledOnce();
        expect(f.released).toHaveBeenCalledOnce();
      }
      const executions = f.proofs.filter(
        (proof) => proof.scopeKind === "execution",
      );
      if (["poll", "reconnect"].includes(method))
        expect(executions).toEqual([]);
      else {
        expect(executions.length).toBe(method === "prove" ? 2 : 1);
        expect(
          executions.every((proof) => proof.proofId === executions[0]!.proofId),
        ).toBe(true);
      }
    },
  );
});

it("retains a retired request until its adapter actually exits, even after Runtime closure", async () => {
  const parent = new Lifetime("channel owner", {
    tasks: 2,
    resources: 1,
    children: 1,
  });
  const reply = Promise.withResolvers<Response>();
  const started = Promise.withResolvers<void>();
  let attempt: AbortSignal | undefined;
  const control = new HTTPControl({
    endpoint: "http://localhost/control",
    credential: "test credential",
    maximumMessageBytes: 4096,
    lifetime: parent,
    fetch(_input, init) {
      attempt = init!.signal!;
      started.resolve();
      return reply.promise;
    },
  });
  const runtime = new Runtime({
    authority,
    realm,
    control,
    proofCapacity: 1,
    load: () => {
      throw new Error("unexpected preparation");
    },
    remote: () => {
      throw new Error("unexpected remote binding");
    },
  });
  const schedule = vi.fn();
  const polling = runtime.poll(schedule);
  await started.promise;
  await runtime.close();
  await polling;
  expect(schedule).not.toHaveBeenCalled();
  expect(attempt!.aborted).toBe(true);
  const closing = control.close();
  const replacement = () =>
    new HTTPControl({
      endpoint: "http://localhost/control",
      credential: "replacement credential",
      maximumMessageBytes: 4096,
      lifetime: parent,
    });
  try {
    expect(replacement).toThrow(expect.objectContaining({ code: "capacity" }));
  } finally {
    reply.resolve(Response.json({ authority, realm, plans: [], fences: [] }));
    expect((await closing).status).toBe("released");
    const next = replacement();
    expect((await next.close()).status).toBe("released");
    expect((await parent.close()).status).toBe("released");
  }
});
