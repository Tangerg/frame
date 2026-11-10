import { FrameError, Lifetime, type Unit } from "@frame/sdk";
import { parseJSON, projectNumber, stringifyJSON } from "@frame/sdk/json";
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
  decodeHTTPOutput,
  httpOperations,
  httpRequestPath,
  type HTTPInputs,
  type HTTPOutputs,
  type Bootstrap,
  type DocumentRef,
} from "./contracts.generated";
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
    return {
      bytes,
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
  const request = async <K extends keyof HTTPInputs>(
    owner: Lifetime,
    operation: K,
    input: NoInfer<HTTPInputs[K]>,
  ): Promise<HTTPOutputs[K]> => {
    const path = httpRequestPath(operation, input);
    const contract = httpOperations[operation];
    return owner.run(async (signal) => {
      let response: Response;
      let body: string;
      try {
        response = await fetch(new URL(path, origin), {
          method: contract.method,
          signal,
          redirect: "error",
          credentials: "omit",
          headers: {
            Authorization: `Bearer ${credential}`,
            "Content-Type": "application/json",
          },
          ...(contract.method === "POST" ? { body: stringifyJSON(input) } : {}),
        });
        body =
          response.body === null
            ? ""
            : new TextDecoder("utf-8", { fatal: true }).decode(
                await readResponseBytes(response, maximumMessageBytes),
              );
      } catch (cause) {
        throw new FrameError(
          contract.method === "GET" ? "unavailable" : "outcome_unknown",
          "author application response was not observed",
          { cause },
        );
      }
      if (!response.ok) throw new Error(body);
      try {
        if (
          response.status !== contract.status ||
          (contract.status === noContentStatus && body !== "")
        )
          throw new TypeError("application response status is invalid");
        return decodeHTTPOutput(
          operation,
          body === "" ? null : parseJSON(body),
        );
      } catch (cause) {
        throw new FrameError(
          contract.method === "GET" ? "unavailable" : "outcome_unknown",
          "author application response is invalid",
          { cause },
        );
      }
    });
  };

  const release = async (): Promise<void> => {
    await Promise.all([...conversions].map((task) => task.close()));
    // Cancelling an activation only stops its waiter. Seal the original Realm
    // before stopping the pump, including plans the Runtime has not observed.
    await request(transport, "sealRealm", {});
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
      await request(application, "activate", { unit });
    },
    activity() {
      return request(application, "activity", undefined);
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
