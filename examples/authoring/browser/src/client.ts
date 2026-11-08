import { FrameError, Lifetime, type Unit } from "@frame/sdk";
import { projectNumber } from "@frame/sdk/json";
import {
  ControlPump,
  readResponseBytes,
  HTTPControl,
  RealmModules,
  Runtime,
} from "@frame/sdk/browser";
import type { Execution } from "@frame/sdk/protocol";
import {
  applicationPaths,
  type Bootstrap,
  type DocumentRef,
} from "./contracts.generated";
import { sha256 } from "./contracts";
import { ConversionTask } from "./conversion-task";
import { ConnectionFailures } from "./failures";

export { converters, validators, sha256 } from "./contracts";
export { ConversionTask, type RenderedDocument } from "./conversion-task";

const maximumModuleBytes = 16 << 20;
const maximumConversionTasks = 128;
const noContentStatus = 204;

export function connect(
  bootstrap: Bootstrap,
  observeError?: (error: unknown) => unknown,
) {
  if (observeError !== undefined && typeof observeError !== "function")
    throw new TypeError("connection error observer must be a function");
  const origin = bootstrap.origin;
  const credential = bootstrap.credentials.token;
  const maximumMessageBytes = projectNumber(
    bootstrap.credentials.maxMessageBytes,
  );
  const application = new Lifetime("author application requests");
  const transport = new Lifetime("author control transport");
  const control = new HTTPControl({
    endpoint: new URL(applicationPaths.control, origin).href,
    credential,
    maximumMessageBytes,
    lifetime: transport,
  });
  const modules = new RealmModules(async (unit, signal) => {
    const response = await fetch(new URL(unit.module.path, origin), {
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
  let closeWork: Promise<void> | undefined;
  const failures = new ConnectionFailures();
  const report = (error: unknown) => {
    failures.report(error, observeError);
  };
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
    onError: report,
  });
  const pumping = pump.run().catch((error) => failures.record(error));
  const request = async (
    owner: Lifetime,
    path: string,
    input?: unknown,
  ): Promise<string> =>
    owner.run(async (signal) => {
      let response: Response;
      let body: string;
      try {
        response = await fetch(new URL(path, origin), {
          method: input === undefined ? "GET" : "POST",
          signal,
          redirect: "error",
          credentials: "omit",
          headers: {
            Authorization: `Bearer ${credential}`,
            "Content-Type": "application/json",
          },
          ...(input === undefined ? {} : { body: JSON.stringify(input) }),
        });
        body =
          response.status === noContentStatus && response.body === null
            ? ""
            : new TextDecoder("utf-8", { fatal: true }).decode(
                await readResponseBytes(response, maximumMessageBytes),
              );
      } catch (cause) {
        throw new FrameError(
          input === undefined ? "unavailable" : "outcome_unknown",
          "author application response was not observed",
          { cause },
        );
      }
      if (!response.ok) throw new Error(body);
      if (
        input !== undefined &&
        (response.status !== noContentStatus || body !== "")
      )
        throw new FrameError(
          "outcome_unknown",
          "author command acknowledgement is invalid",
        );
      return body;
    });

  const release = async (): Promise<void> => {
    await Promise.all([...conversions].map((task) => task.close()));
    // Cancelling an activation only stops its waiter. Seal the original Realm
    // before stopping the pump, including plans the Runtime has not observed.
    await request(transport, applicationPaths.sealRealm, {});
    await pump.close();
    await pumping;
    await application.wait();
    await transport.close();
  };

  return {
    runtime,
    control,
    get failures() {
      return failures.records;
    },
    get omittedFailures() {
      return failures.omitted;
    },
    async activate(unit: string): Promise<void> {
      await request(application, applicationPaths.activate, { unit });
    },
    async activity(): Promise<{ started: number; cancelled: number }> {
      return JSON.parse(await request(application, applicationPaths.activity));
    },
    createConversion(reference: DocumentRef): ConversionTask {
      application.assertOpen();
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
    close(): Promise<void> {
      if (closeWork) return closeWork;
      const completion = Promise.withResolvers<void>();
      closeWork = completion.promise.catch((error) => {
        closeWork = undefined;
        throw error;
      });
      application.beginClose();
      release().then(completion.resolve, completion.reject);
      return closeWork;
    },
  };
}
