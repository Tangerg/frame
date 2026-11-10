import { Point, type Lifetime, type Unit } from "@frame/sdk";
import {
  ControlPump,
  readResponseBytes,
  RealmModules,
  Runtime,
  select,
  type HTTPControl,
  type Use,
} from "@frame/sdk/browser";
import { compareJSONNumbers } from "@frame/sdk/json";
import type {
  AcquireRequest,
  ContractRef,
  Execution,
} from "@frame/sdk/protocol";
import {
  decodeWireValue,
  type Converted,
  type Credentials,
  type PreviewMetadata,
} from "./api.generated.ts";
import { DocumentServer } from "./document-server.ts";
import { PreviewSnapshots } from "./preview-capture.ts";
import { SerialActions } from "./serial-actions.ts";

const maximumPreviewModuleBytes = 1024 * 1024;
const proofCapacity = 512;
const pollIntervalMilliseconds = 100;
const preparationCapacity = 1;
const selectionRule = "documents.html-preview-priority/1";
const previewFormat = "html";

interface Preview {
  mount(
    element: HTMLElement,
    document: Converted,
    theme: string,
    owner: Lifetime,
  ): void;
}

type PreviewUse =
  | { status: "acquiring"; request: AcquireRequest }
  | { status: "mounted"; use: Use };

export class PreviewSession {
  readonly #server: DocumentServer;
  readonly #runtime: Runtime;
  readonly #point: Point<PreviewMetadata, Preview>;
  readonly #snapshots: PreviewSnapshots;
  readonly #previews: SerialActions;
  readonly #pump: ControlPump;
  readonly #pumpWork: Promise<void>;
  #credentials: Credentials;
  readonly #controls = new Set<HTTPControl>();
  #activation?: { status: "preparing" | "active"; execution: Execution };
  #use?: PreviewUse;

  constructor(
    server: DocumentServer,
    credentials: Credentials,
    contract: ContractRef,
    actions: Lifetime,
    report: (error: unknown) => void,
  ) {
    this.#server = server;
    this.#credentials = credentials;
    this.#point = new Point(contract);
    const control = server.control(credentials);
    this.#controls.add(control);
    this.#snapshots = new PreviewSnapshots(this.#point.ref.name);
    this.#previews = new SerialActions(actions);
    const modules = new RealmModules(async (unit, signal) => {
      const response = await fetch(unit.module.path, {
        signal,
        credentials: "omit",
        redirect: "error",
      });
      if (!response.ok) throw new Error("approved module is unavailable");
      return {
        bytes: await readResponseBytes(response, maximumPreviewModuleBytes),
        close() {},
      };
    });
    this.#runtime = new Runtime({
      authority: credentials.authority,
      realm: credentials.realm,
      control,
      proofCapacity,
      load: async (unit, owner, plan): Promise<Unit> => {
        if (!this.#activation)
          this.#activation = { status: "preparing", execution: plan.execution };
        else if (
          this.#activation.execution.id !== plan.execution.id ||
          this.#activation.execution.generation !== plan.execution.generation
        )
          throw new Error("the preview session already owns another execution");
        return modules.load(unit, owner, plan);
      },
      remote: async () => {
        throw new Error("this consumer has no remote import contract");
      },
    });
    this.#pump = new ControlPump(this.#runtime, {
      intervalMilliseconds: pollIntervalMilliseconds,
      preparationCapacity,
      onError: report,
    });
    this.#pumpWork = this.#pump.run().catch(report);
  }

  get description(): string {
    const credentials = this.#credentials;
    return `Realm ${credentials.realm.id.slice(0, 12)} / Host epoch ${credentials.authority.epoch}`;
  }

  async activate(signal: AbortSignal): Promise<void> {
    this.#assertConnected();
    // Host activation joins the original instance after a lost reply.
    await this.#server.request("activatePreview", this.#credentials, signal);
    signal.throwIfAborted();
    if (!this.#activation)
      throw new Error(
        "preview activation has not completed; reconnect its Realm",
      );
    this.#activation = {
      status: "active",
      execution: this.#activation.execution,
    };
  }

  async reconnect(signal: AbortSignal): Promise<string> {
    if (this.#runtime.connection === "closed")
      throw new Error("reload this page before opening another Realm");
    await this.#resumeControl(signal);
    if (this.#runtime.connection === "connected") {
      await this.#previews.run(() => this.#recover());
      await this.activate(signal);
    }
    return `Original Realm: ${this.#runtime.connection}`;
  }

  async #resumeControl(signal?: AbortSignal): Promise<void> {
    const original = this.#credentials;
    const credentials = await this.#server.request(
      "resumeRealm",
      { realm: original.realm, resumeToken: original.resumeToken },
      signal,
    );
    signal?.throwIfAborted();
    const control = this.#server.control(credentials);
    this.#controls.add(control);
    this.#credentials = credentials;
    const failures: unknown[] = [];
    try {
      await this.#runtime.reconnect(control);
    } catch (error) {
      failures.push(error);
    }
    try {
      await this.#closeControls(
        [...this.#controls].filter((owned) => owned !== this.#runtime.control),
      );
    } catch (error) {
      failures.push(error);
    }
    if (failures.length === 1) throw failures[0];
    if (failures.length)
      throw new AggregateError(failures, "control channel replacement failed");
    signal?.throwIfAborted();
  }

  mount(
    document: Converted,
    target: HTMLElement,
    current: () => boolean,
  ): Promise<void> {
    return this.#previews.run((signal) =>
      this.#mount(document, target, current, signal),
    );
  }

  async close(): Promise<void> {
    // Action admission stays sealed. A close retry refreshes only the original
    // Realm's cleanup channel, including after Host credential rotation.
    if (this.#runtime.connection === "closed") await this.#resumeControl();
    const settled = await Promise.allSettled([
      this.#server.request("sealRealm", {
        token: this.#credentials.token,
      }),
      this.#pump.close(),
      this.#pumpWork,
    ]);
    const failures = settled.flatMap((result) =>
      result.status === "rejected" ? [result.reason] : [],
    );
    if (failures.length)
      throw new AggregateError(
        failures,
        "original preview session release did not complete",
      );
    await this.#closeControls([...this.#controls]);
  }

  async #closeControls(controls: readonly HTTPControl[]): Promise<void> {
    const results = await Promise.allSettled(
      controls.map(async (control) => {
        const outcome = await control.close();
        if (outcome.status !== "released")
          throw new Error("original control channel release is unconfirmed");
        this.#controls.delete(control);
      }),
    );
    const failures = results.flatMap((result) =>
      result.status === "rejected" ? [result.reason] : [],
    );
    if (failures.length)
      throw new AggregateError(failures, "control channel cleanup failed");
  }

  #assertConnected(): void {
    if (this.#runtime.connection !== "connected")
      throw new Error("connect the original Realm before using its preview");
  }

  #activatedExecution(): Execution {
    if (this.#activation?.status !== "active")
      throw new Error(
        "preview activation has not completed; reconnect its Realm",
      );
    return this.#activation.execution;
  }

  async #closeSnapshotAfterFailure(failure: unknown): Promise<never> {
    try {
      await this.#snapshots.close(this.#runtime.control);
    } catch (cleanup) {
      throw new AggregateError(
        [failure, cleanup],
        "preview selection and cleanup failed",
      );
    }
    throw failure;
  }

  async #acquire(request: AcquireRequest): Promise<Use> {
    this.#use = { status: "acquiring", request };
    try {
      const use = await this.#runtime.acquire(request);
      this.#snapshots.consumed(request.snapshotId);
      this.#use = { status: "mounted", use };
      return use;
    } catch (error) {
      // An uncertain grant keeps its original operation and snapshot together.
      if (this.#runtime.hasPendingAcquisition(request.operationId)) throw error;
      this.#use = undefined;
      return this.#closeSnapshotAfterFailure(error);
    }
  }

  async #closeUse(): Promise<void> {
    if (!this.#use) return;
    if (this.#use.status === "acquiring")
      throw new Error(
        "recover the original preview acquisition before closing its Use",
      );
    const outcome = await this.#use.use.close();
    if (outcome.status !== "released")
      throw new Error("preview release is unconfirmed");
    await this.#runtime.flushProofs();
    this.#use = undefined;
  }

  async #recover(): Promise<void> {
    if (this.#use?.status === "acquiring") {
      await this.#acquire(this.#use.request);
      await this.#closeUse();
    }
    await this.#snapshots.close(this.#runtime.control);
  }

  async #mount(
    document: Converted,
    target: HTMLElement,
    current: () => boolean,
    signal: AbortSignal,
  ): Promise<void> {
    if (!current()) return;
    this.#assertConnected();
    const execution = this.#activatedExecution();
    await this.#recover();
    await this.#closeUse();
    signal.throwIfAborted();
    const snapshot = await this.#snapshots.capture(this.#runtime.control);
    let choice;
    try {
      choice = select(
        snapshot,
        this.#point,
        selectionRule,
        (choices) =>
          [...choices]
            .filter((choice) => choice.metadata.format === previewFormat)
            .sort((a, b) =>
              compareJSONNumbers(b.metadata.priority, a.metadata.priority),
            )
            .slice(0, 1),
        (value) => decodeWireValue("PreviewMetadata", value),
      );
    } catch (error) {
      return this.#closeSnapshotAfterFailure(error);
    }
    const use = await this.#acquire({
      operationId: crypto.randomUUID(),
      snapshotId: snapshot.id,
      execution,
      selections: [choice],
    });
    try {
      signal.throwIfAborted();
      const selected = snapshot.points[0]?.candidates.find(
        (candidate) => candidate.token === choice.candidates[0],
      );
      if (!selected) throw new Error("no approved preview is available");
      const theme = await this.#server.request("theme", undefined, signal);
      signal.throwIfAborted();
      const object = use.objects.find(
        (object) =>
          object.export.name === selected.source.export &&
          object.export.contract.digest === this.#point.ref.digest,
      );
      if (!object)
        throw new Error("the original Use does not own the selected preview");
      await use.with(object, this.#point, (preview) => {
        if (current()) preview.mount(target, document, theme.css, use.lifetime);
      });
      if (!current()) await this.#closeUse();
    } catch (error) {
      try {
        await this.#closeUse();
      } catch (cleanup) {
        throw new AggregateError(
          [error, cleanup],
          "preview preparation and cleanup failed",
        );
      }
      throw error;
    }
  }
}
