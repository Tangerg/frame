import {
  acquire,
  Lifetime,
  sameContract,
  type Acquired,
  type CloseOutcome,
  type Unit,
} from "@frame/sdk";
import { projectNumber } from "@frame/sdk/json";
import {
  ControlPump,
  HTTPControl,
  RealmModules,
  Runtime,
  select,
  type Use,
} from "@frame/sdk/browser";
import type { Execution } from "@frame/sdk/protocol";
import type {
  Bootstrap,
  Converted,
  Diagnostic,
  DocumentRef,
} from "./contracts.generated";
import { converters, validators, sha256 } from "./contracts";

export { converters, validators, sha256 } from "./contracts";

export interface RenderedDocument {
  readonly document: Converted;
  readonly diagnostics: readonly Diagnostic[];
  readonly url: string;
}

export interface ConversionTask {
  readonly result: Promise<Acquired<RenderedDocument>>;
  close(signal?: AbortSignal): Promise<CloseOutcome>;
}

/** The application composes original values; plugin authors only define Units. */
export function connect(bootstrap: Bootstrap) {
  const transport = new Lifetime("author control transport");
  const control = new HTTPControl({
    endpoint: new URL("/frame/control", bootstrap.origin).href,
    credential: bootstrap.credentials.token,
    maximumMessageBytes: 8 << 20,
    lifetime: transport,
  });
  const modules = new RealmModules(async (unit, signal) => {
    const response = await fetch(new URL(unit.module.path, bootstrap.origin), {
      signal,
    });
    if (!response.ok) throw new Error("approved module is unavailable");
    const bytes = new Uint8Array(await response.arrayBuffer());
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
  const stop = new AbortController();
  const pump = new ControlPump(runtime, control, {
    intervalMilliseconds: 5,
    onError(error) {
      errors.push(error);
    },
  });
  const pumping = pump.run(stop.signal);
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
    async start(reference: DocumentRef): Promise<ConversionTask> {
      const execution = executions.get("converter");
      if (!execution) throw new Error("activate the Converter before using it");
      // Capture both Points once. Ordering is explicit application policy.
      const snapshot = await control.snapshot([
        converters.ref.name,
        validators.ref.name,
      ]);
      try {
        const conversion = select(
          snapshot,
          converters,
          "author.markdown/1",
          (choices) =>
            choices
              .filter((entry) => entry.metadata.format === "markdown")
              .slice(0, 1),
          (value) => {
            const metadata = value as { format?: unknown; priority?: unknown };
            if (typeof metadata.format !== "string")
              throw new TypeError("invalid Converter metadata");
            return {
              format: metadata.format,
              priority: projectNumber(metadata.priority),
            };
          },
        );
        const validation = select(
          snapshot,
          validators,
          "author.validation-order/1",
          (choices) =>
            [...choices].sort(
              (left, right) => left.metadata.order - right.metadata.order,
            ),
          (value) => {
            const metadata = value as { name?: unknown; order?: unknown };
            if (typeof metadata.name !== "string")
              throw new TypeError("invalid Validator metadata");
            return {
              name: metadata.name,
              order: projectNumber(metadata.order),
            };
          },
        );
        if (
          conversion.candidates.length !== 1 ||
          validation.candidates.length === 0
        )
          throw new Error("the author composition requires both Points");
        const use = await runtime.acquire({
          operationId: crypto.randomUUID(),
          snapshotId: snapshot.id,
          execution,
          selections: [conversion, validation],
        });
        const result = convert(use, reference);
        return { result, close: (signal) => use.close(signal) };
      } catch (error) {
        try {
          await control.closeSnapshot(snapshot.id);
        } catch (closeFailure) {
          throw new AggregateError(
            [error, closeFailure],
            "snapshot selection and cleanup failed",
          );
        }
        throw error;
      }
    },
    async close(): Promise<void> {
      stop.abort();
      await pumping;
      const outcomes = await runtime.close();
      if (outcomes.some((outcome) => outcome.status !== "released"))
        throw new Error("original author execution did not release");
      await transport.close();
    },
  };
}

function convert(
  use: Use,
  reference: DocumentRef,
): Promise<Acquired<RenderedDocument>> {
  // Register the complete composition before starting the first fetch. Closing
  // this Use aborts and joins conversion, validation and late resource creation.
  return use.lifetime.run(async () => {
    const source = use.objects.find((object) =>
      sameContract(object.export.contract, converters.ref),
    );
    if (!source) throw new Error("the approved Use has no Converter");
    const document = await use.with(source, converters, (convert, signal) =>
      convert(reference, signal),
    );
    const diagnostics: Diagnostic[] = [];
    for (const object of use.objects) {
      if (sameContract(object.export.contract, validators.ref))
        diagnostics.push(
          ...(await use.with(object, validators, (validate, signal) =>
            validate(document, signal),
          )),
        );
    }
    const acquired = await acquire(
      use.lifetime,
      "converted HTML Blob URL",
      () => {
        const url = URL.createObjectURL(
          new Blob([document.html], { type: "text/html" }),
        );
        return {
          value: { document, diagnostics, url },
          close() {
            URL.revokeObjectURL(url);
          },
        };
      },
    );
    // This convenience handle closes the original approved Use, including its
    // result resource. It creates no independently advancing exit owner.
    return Object.freeze({
      with: acquired.with,
      close: (signal?: AbortSignal) => use.close(signal),
      wait: (signal?: AbortSignal) => use.wait(signal),
    });
  });
}
