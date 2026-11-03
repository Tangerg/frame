import { FrameError, Lifetime, type Unit } from "@frame/sdk";
import {
  ControlPump,
  readResponseBytes,
  HTTPControl,
  RealmModules,
  Runtime,
} from "@frame/sdk/browser";
import type { Execution } from "@frame/sdk/protocol";
import type { Bootstrap, DocumentRef } from "./contracts.generated";
import { sha256 } from "./contracts";
import { ConversionTask } from "./conversion-task";

export { converters, validators, sha256 } from "./contracts";
export { ConversionTask, type RenderedDocument } from "./conversion-task";

const maximumModuleBytes = 16 << 20;
const maximumConversionTasks = 128;

export function connect(bootstrap: Bootstrap) {
  const transport = new Lifetime("author control transport");
  const control = new HTTPControl({
    endpoint: new URL("/frame/control", bootstrap.origin).href,
    credential: bootstrap.credentials.token,
    maximumMessageBytes: bootstrap.credentials.maxMessageBytes,
    lifetime: transport,
  });
  const modules = new RealmModules(async (unit, signal) => {
    const response = await fetch(new URL(unit.module.path, bootstrap.origin), {
      signal,
      redirect: "error",
      credentials: "omit",
    });
    if (!response.ok) throw new Error("approved module is unavailable");
    const bytes = await readResponseBytes(response, maximumModuleBytes);
    const digest = await sha256(bytes);
    // Node's native ESM loader does not import HTTP. This local test loader
    // imports only the verified original bytes; actual browser evidence lives
    // in the separate document workbench's browser-engine workflow.
    let binary = "";
    for (const value of bytes) binary += String.fromCharCode(value);
    return {
      url: `data:text/javascript;base64,${btoa(binary)}`,
      digest,
      close() {},
    };
  });
  // This is a projection of identities delivered by the Host, not an instance
  // directory or a provider selector. Runtime owns their objects and exits.
  const executions = new Map<string, Execution>();
  const conversions = new Set<ConversionTask>();
  let closing = false;
  const errors: unknown[] = [];
  const runtime = new Runtime({
    authority: bootstrap.credentials.authority,
    realm: bootstrap.credentials.realm,
    control,
    proofCapacity: 128,
    async load(unit, owner, plan) {
      const exported = await modules.evaluate(unit, owner, plan);
      const definitions = exported.units as readonly Unit[] | undefined;
      const definition = definitions?.find(
        (entry) => entry.id === unit.instance.source.unitId,
      );
      if (!definition)
        throw new Error("approved module does not export its Unit");
      executions.set(definition.id, plan.execution);
      return definition;
    },
    remote() {
      throw new Error(
        "the author exercise declares only same-Realm native imports",
      );
    },
  });
  const pump = new ControlPump(runtime, {
    intervalMilliseconds: 5,
    preparationCapacity: 4,
    onError(error) {
      errors.push(error);
    },
  });
  const pumping = pump.run().catch((error) => errors.push(error));
  const request = async (path: string, input?: unknown): Promise<Response> =>
    fetch(new URL(path, bootstrap.origin), {
      method: input === undefined ? "GET" : "POST",
      headers: {
        Authorization: `Bearer ${bootstrap.credentials.token}`,
        "Content-Type": "application/json",
      },
      ...(input === undefined ? {} : { body: JSON.stringify(input) }),
    });

  return {
    runtime,
    control,
    errors,
    async activate(unit: string): Promise<void> {
      const response = await request("/activate", { unit });
      if (!response.ok) throw new Error(await response.text());
    },
    async activity(): Promise<{ started: number; cancelled: number }> {
      const response = await request("/activity");
      if (!response.ok) throw new Error(await response.text());
      return response.json();
    },
    createConversion(reference: DocumentRef): ConversionTask {
      if (closing) throw new FrameError("closed", "author client is closing");
      if (conversions.size >= maximumConversionTasks)
        throw new FrameError(
          "capacity",
          "author conversion task capacity reached",
        );
      const execution = executions.get("converter");
      if (!execution) throw new Error("activate the Converter before using it");
      const task = new ConversionTask(
        runtime,
        control,
        execution,
        reference,
        (settled) => {
          conversions.delete(settled);
        },
      );
      conversions.add(task);
      return task;
    },
    async close(): Promise<void> {
      closing = true;
      await Promise.all([...conversions].map((task) => task.close()));
      await pump.close();
      await pumping;
      await transport.close();
    },
  };
}
