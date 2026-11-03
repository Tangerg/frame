import { acquire, FrameError, sameContract, type Acquired } from "@frame/sdk";
import { projectNumber } from "@frame/sdk/json";
import {
  select,
  isControlRefusal,
  type HostControl,
  type Runtime,
  type Use,
} from "@frame/sdk/browser";
import type {
  AcquireRequest,
  Execution,
  Snapshot,
  SnapshotRequest,
} from "@frame/sdk/protocol";
import type { Converted, Diagnostic, DocumentRef } from "./contracts.generated";
import { converters, validators, markdownFormat } from "./contracts";

const markdownSelectionRule = "author.markdown/1";
const validationOrderRule = "author.validation-order/1";

export interface RenderedDocument {
  readonly document: Converted;
  readonly diagnostics: readonly Diagnostic[];
  readonly url: string;
}

export class ConversionTask {
  readonly #operationId = crypto.randomUUID();
  readonly #capture: SnapshotRequest = Object.freeze({
    operationId: crypto.randomUUID(),
    points: Object.freeze([converters.ref.name, validators.ref.name]),
  });
  readonly #execution: Execution;
  readonly #reference: DocumentRef;
  #runtime?: Runtime;
  #control?: HostControl;
  #request?: AcquireRequest;
  #captureDispatched = false;
  #acquiring?: Promise<Use>;
  #use?: Use;
  #conversion?: Promise<Acquired<RenderedDocument>>;
  #closing = false;
  #closeWork?: Promise<void>;
  #onSettled?: (task: ConversionTask) => void;

  constructor(
    runtime: Runtime,
    control: HostControl,
    execution: Execution,
    reference: DocumentRef,
    onSettled: (task: ConversionTask) => void,
  ) {
    this.#runtime = runtime;
    this.#control = control;
    this.#execution = Object.freeze({ ...execution });
    this.#reference = Object.freeze({ ...reference });
    this.#onSettled = onSettled;
  }

  get operationId(): string {
    return this.#operationId;
  }

  run(): Promise<Acquired<RenderedDocument>> {
    if (this.#closing)
      return Promise.reject(
        new FrameError("closed", "conversion task is closing"),
      );
    if (this.#conversion) return this.#conversion;
    return this.#acquire().then((use) => {
      if (this.#closing)
        throw new FrameError("closed", "conversion task is closing");
      this.#conversion ??= convert(use, this.#reference);
      return this.#conversion;
    });
  }

  close(): Promise<void> {
    this.#closing = true;
    this.#closeWork ??= this.#release().catch((error) => {
      this.#closeWork = undefined;
      throw error;
    });
    return this.#closeWork;
  }

  #acquire(): Promise<Use> {
    if (this.#use) return Promise.resolve(this.#use);
    if (this.#acquiring) return this.#acquiring;
    this.#acquiring = Promise.resolve()
      .then(async () => {
        if (!this.#request) {
          if (this.#closing)
            throw new FrameError("closed", "conversion task is closing");
          const previouslyDispatched = this.#captureDispatched;
          this.#captureDispatched = true;
          let snapshot: Snapshot;
          try {
            snapshot = await this.#control!.snapshot(this.#capture);
          } catch (error) {
            if (!previouslyDispatched && isControlRefusal(error))
              this.#forget();
            throw error;
          }
          if (this.#closing)
            throw new FrameError("closed", "conversion task is closing");
          try {
            this.#request = Object.freeze({
              operationId: this.operationId,
              snapshotId: snapshot.id,
              execution: this.#execution,
              selections: documentSelections(snapshot),
            });
          } catch (error) {
            this.#closing = true;
            try {
              await this.#control!.closeSnapshot(this.#capture.operationId);
            } catch (closeFailure) {
              throw new AggregateError(
                [error, closeFailure],
                "snapshot selection and cleanup failed",
              );
            }
            this.#forget();
            throw error;
          }
        }
        const use = await this.#runtime!.acquire(this.#request);
        this.#use = use;
        use.lifetime.observeExit((outcome) => {
          if (outcome.status === "released") this.#forget();
        });
        return use;
      })
      .finally(() => {
        this.#acquiring = undefined;
      });
    return this.#acquiring;
  }

  async #release(): Promise<void> {
    const control = this.#control;
    const runtime = this.#runtime;
    if (!control || !runtime) return;
    if (!this.#use && this.#captureDispatched)
      await control.closeSnapshot(this.#capture.operationId);
    try {
      await this.#acquiring;
    } catch {
      // Closing resolves the same admission below; run already reports its error.
    }
    if (
      !this.#use &&
      this.#request &&
      runtime.hasPendingAcquisition(this.operationId)
    ) {
      try {
        await this.#acquire();
      } catch (error) {
        if (runtime.hasPendingAcquisition(this.operationId)) throw error;
      }
    }
    if (this.#use) {
      const outcome = await this.#use.close();
      if (outcome.status !== "released")
        throw new FrameError(
          "close_unconfirmed",
          "conversion use has not released",
          { cause: outcome },
        );
    }
    this.#forget();
  }

  #forget(): void {
    this.#closing = true;
    this.#request = undefined;
    this.#use = undefined;
    this.#conversion = undefined;
    this.#control = undefined;
    this.#runtime = undefined;
    const onSettled = this.#onSettled;
    this.#onSettled = undefined;
    onSettled?.(this);
  }
}

function documentSelections(snapshot: Snapshot): AcquireRequest["selections"] {
  const conversion = select(
    snapshot,
    converters,
    markdownSelectionRule,
    (choices) =>
      choices
        .filter((entry) => entry.metadata.format === markdownFormat)
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
    validationOrderRule,
    (choices) =>
      [...choices].sort(
        (left, right) => left.metadata.order - right.metadata.order,
      ),
    (value) => {
      const metadata = value as { name?: unknown; order?: unknown };
      if (typeof metadata.name !== "string")
        throw new TypeError("invalid Validator metadata");
      return { name: metadata.name, order: projectNumber(metadata.order) };
    },
  );
  if (conversion.candidates.length !== 1 || validation.candidates.length === 0)
    throw new Error("the author composition requires both Points");
  return Object.freeze([conversion, validation]);
}

function convert(
  use: Use,
  reference: DocumentRef,
): Promise<Acquired<RenderedDocument>> {
  // Closing the approved Use joins every conversion and late resource acquisition.
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
    return Object.freeze({
      with: acquired.with,
      close: (signal?: AbortSignal) => use.close(signal),
      wait: (signal?: AbortSignal) => use.wait(signal),
    });
  });
}
