import { captureProtocol } from "../protocol-capture.js";
import {
  Lifetime,
  Point,
  Event,
  FrameError,
  sameContract,
  type Awaitable,
  type CloseOutcome,
  type Contract,
  type Setup,
  type Unit,
} from "../index.js";
import { parseJSON } from "../json.js";
import type {
  AcquireRequest,
  Authority,
  CloseProof,
  Execution,
  Import,
  LocalPlan,
  ObjectRef,
  PlannedUnit,
  Poll,
  Realm,
  RouteGrant,
  UseGrant,
} from "../protocol.generated.js";
import { PROTOCOL } from "../protocol.generated.js";
import {
  ControlChannel,
  type ConnectionState,
  type HostControl,
} from "./control.js";
import { observePromise } from "../promise-observation.js";
import {
  sameAuthority,
  sameExecution,
  sameObject,
  sameRealm,
} from "./identity.js";
import {
  Publication,
  type PublicationChannel,
  type PublicationHandle,
} from "./publication.js";
import { ProofOutbox } from "./outbox.js";
import { UseAdmission } from "./admission.js";
import {
  ExecutionResources,
  type ObjectResource,
  type UnitResource,
} from "./execution.js";

const SUPERSEDED_REVALIDATION = Object.freeze(
  new FrameError("unavailable", "control revalidation was superseded"),
);

export interface RuntimeOptions {
  readonly authority: Authority;
  readonly realm: Realm;
  readonly control: HostControl;
  readonly proofCapacity: number;
  load(unit: PlannedUnit, lifetime: Lifetime, plan: LocalPlan): Awaitable<Unit>;
  remote(binding: Import, lifetime: Lifetime): Awaitable<NonNullable<unknown>>;
}

export interface ExecutionHandle {
  readonly identity: Execution;
  readonly objects: readonly ObjectRef[];
  close(signal?: AbortSignal): Promise<CloseOutcome>;
  wait(signal?: AbortSignal): Promise<CloseOutcome>;
}

export interface Use {
  readonly id: string;
  readonly objects: readonly ObjectRef[];
  readonly lifetime: Lifetime;
  with<T, R>(
    reference: ObjectRef,
    contract: Contract<T>,
    work: (value: T, signal: AbortSignal) => Awaitable<R>,
  ): Promise<R>;
  close(signal?: AbortSignal): Promise<CloseOutcome>;
  wait(signal?: AbortSignal): Promise<CloseOutcome>;
}

export interface EventRoute<T> {
  dispatch(payload: T): Promise<void>;
  close(signal?: AbortSignal): Promise<CloseOutcome>;
}

function useView(
  grant: UseGrant,
  owner: Lifetime,
  objects: ReadonlyMap<string, ObjectResource>,
  admission: () => void,
): Use {
  let admit: (() => void) | undefined = admission;
  owner.observeExit((outcome) => {
    if (outcome.status === "released") admit = undefined;
  });
  return Object.freeze({
    id: grant.id,
    objects: grant.objects,
    lifetime: owner,
    with: <T, R>(
      reference: ObjectRef,
      contract: Contract<T>,
      work: (value: T, signal: AbortSignal) => Awaitable<R>,
    ) => {
      owner.assertOpen();
      admit!();
      const object = objects.get(reference.objectId);
      if (
        !object ||
        !sameObject(reference, object.reference) ||
        !sameContract(contract.ref, reference.export.contract)
      )
        throw new FrameError(
          "permission_denied",
          "value is outside this use grant",
        );
      return owner.run((signal) => work(object.value as T, signal));
    },
    close: (signal?: AbortSignal) => owner.close(signal),
    wait: (signal?: AbortSignal) => owner.wait(signal),
  });
}

function eventRouteView<T>(
  use: Use,
  state: {
    listener?: (payload: T, signal: AbortSignal) => Awaitable<void>;
    admit?: () => void;
  },
): EventRoute<T> {
  use.lifetime.cleanup(() => {
    state.listener = undefined;
    state.admit = undefined;
  }, "event-handler");
  return Object.freeze({
    dispatch: async (payload: T) => {
      if (!state.listener || !state.admit)
        throw new FrameError("closed", "event listener is closed");
      state.admit();
      const captured = state.listener;
      await use.lifetime.run((signal) => captured(payload, signal));
    },
    close: (signal?: AbortSignal) => use.close(signal),
  });
}

export class Runtime {
  readonly #realm: Realm;
  #channel: ControlChannel;
  readonly #load: RuntimeOptions["load"];
  readonly #remote: RuntimeOptions["remote"];
  readonly #outbox: ProofOutbox;
  #connection: ConnectionState = "connected";
  #connectionRevision = 0;
  #revalidation?: {
    revision: number;
    work: Promise<void>;
    stopping: AbortController;
  };
  #executionProjectionRevision = 0;
  #executions = new Map<string, ExecutionResources>();
  readonly #instances = new Map<string, UnitResource>();
  readonly #objects = new Map<string, ObjectResource>();
  #uses = new Map<string, UseAdmission>();
  #admissions = new Map<string, UseAdmission>();
  #closing?: Promise<readonly CloseOutcome[]>;

  constructor(options: RuntimeOptions) {
    const { realm, authority, control, load, remote, proofCapacity } = options;
    if (typeof load !== "function" || typeof remote !== "function")
      throw new TypeError("runtime loaders must be functions");
    this.#realm = captureProtocol("Realm", realm);
    this.#channel = new ControlChannel(authority, control);
    this.#load = load.bind(options);
    this.#remote = remote.bind(options);
    this.#outbox = new ProofOutbox(proofCapacity);
  }

  get authority(): Authority {
    return this.#authority;
  }
  get #authority(): Authority {
    return this.#channel.authority;
  }
  get control(): HostControl {
    return this.#channel.control;
  }
  get realm(): Realm {
    return this.#realm;
  }
  get connection(): ConnectionState {
    return this.#connection;
  }
  get pendingProofs(): readonly CloseProof[] {
    return this.#outbox.pending;
  }

  #assertConnected(): void {
    if (this.#connection !== "connected")
      throw new FrameError(
        "unavailable",
        "runtime accepts no new framework work on this control channel",
      );
  }

  #assertRevision(revision: number): void {
    this.#assertConnected();
    if (revision !== this.#connectionRevision)
      throw new FrameError(
        "unavailable",
        "preparation belongs to a retired control channel",
      );
  }

  async prepare(received: LocalPlan): Promise<ExecutionHandle> {
    const plan = captureProtocol("LocalPlan", received);
    const existing = this.#executions.get(plan.execution.id);
    if (existing) {
      existing.verify(plan);
      return this.#resumePreparation(
        existing,
        this.#channel,
        this.#connectionRevision,
      );
    }
    this.#assertConnected();
    if (
      plan.protocol !== PROTOCOL ||
      plan.units.some((unit) => unit.profile !== "browser-js")
    )
      throw new FrameError(
        "contract_mismatch",
        "plan uses an unsupported browser execution profile",
      );
    if (
      !sameAuthority(plan.authority, this.#authority) ||
      !sameRealm(plan.realm, this.#realm)
    )
      throw new FrameError(
        "permission_denied",
        "execution belongs to another authority or realm",
      );
    const entry = new ExecutionResources(
      plan,
      this.#instances,
      this.#objects,
      this.#outbox,
      (settled) => this.#forgetExecution(settled),
    );
    this.#executions.set(plan.execution.id, entry);
    return this.#resumePreparation(
      entry,
      this.#channel,
      this.#connectionRevision,
    );
  }

  #resumePreparation(
    entry: ExecutionResources,
    channel: ControlChannel,
    revision: number,
  ): Promise<ExecutionHandle> {
    return entry.prepare(channel, async () => {
      if (entry.lifetime.closing && !entry.admission.pending)
        throw new FrameError("stale_binding", "original execution is closing");
      if (!entry.prepared) await this.#prepare(entry, channel, revision);
      this.#assertRevision(revision);
      const view = await entry.submitReady(channel);
      this.#assertRevision(revision);
      return view;
    });
  }

  async #prepare(
    entry: ExecutionResources,
    channel: ControlChannel,
    revision: number,
  ): Promise<void> {
    const admit = () => entry.admission.resolve(channel);
    const decision = await (entry.lifetime.closing
      ? admit()
      : entry.lifetime.run(admit));
    if (decision !== "admitted") {
      await entry.lifetime.close();
      throw new FrameError(
        "stale_binding",
        "host admission does not authorize preparation",
      );
    }
    try {
      this.#assertRevision(revision);
      entry.lifetime.assertOpen();
      entry.ownUnits();
      await entry.lifetime.run(async () => {
        for (const unit of entry.ordered)
          await this.#prepareUnit(entry, unit, revision);
      });
      this.#assertRevision(revision);
    } catch (error) {
      await entry.lifetime.close();
      throw error;
    }
    entry.finishPreparation();
  }

  async #prepareUnit(
    entry: ExecutionResources,
    unit: PlannedUnit,
    revision: number,
  ): Promise<void> {
    const owner = entry.unit(unit.instance.id)!.lifetime;
    const live = () => {
      this.#assertRevision(revision);
      owner.assertOpen();
    };
    live();
    const definition = await owner.run(() =>
      this.#load(unit, owner, entry.plan),
    );
    live();
    entry.verifyDefinition(unit, definition);
    const imports = await this.#bindImports(unit, definition, owner, live);
    const context: Setup<typeof definition.imports> = Object.freeze({
      imports: Object.freeze(imports),
      configuration: parseJSON(unit.configurationJson),
      lifetime: owner,
    });
    this.#assertRevision(revision);
    const exports = await owner.run(() => definition.setup(context));
    live();
    if (!exports || Object.keys(exports).length !== unit.exports.length)
      throw new FrameError(
        "contract_mismatch",
        "actual export set differs from its declaration",
      );
    entry.stageExports(unit, definition, exports);
  }

  async #bindImports(
    unit: PlannedUnit,
    definition: Unit,
    owner: Lifetime,
    live: () => void,
  ): Promise<Record<string, unknown>> {
    const imports: Record<string, unknown> = Object.create(null);
    if (Object.keys(definition.imports).length !== unit.imports.length)
      throw new FrameError(
        "contract_mismatch",
        "module imports differ from the approved slots",
      );
    for (const binding of unit.imports) {
      const declared = definition.imports[binding.slot];
      if (!Object.hasOwn(definition.imports, binding.slot) || !declared)
        throw new FrameError(
          "contract_mismatch",
          "module is missing an approved import slot",
        );
      const token = "contract" in declared ? declared.contract : declared;
      if (!sameContract(token.ref, binding.contract))
        throw new FrameError(
          "contract_mismatch",
          "import slot contract differs from the approved binding",
        );
      if (binding.kind === "absent") {
        if (!("optional" in declared && declared.optional))
          throw new FrameError(
            "contract_mismatch",
            "required import was approved as absent",
          );
        imports[binding.slot] = undefined;
      } else if (binding.kind === "local") {
        imports[binding.slot] = this.#localImport(binding.local!);
      } else {
        const implementation = await owner.run(() =>
          this.#remote(binding, owner),
        );
        live();
        if (implementation === null || implementation === undefined)
          throw new FrameError(
            "contract_mismatch",
            "remote import is missing its approved implementation",
          );
        imports[binding.slot] = implementation;
      }
    }
    return imports;
  }

  #localImport(local: NonNullable<Import["local"]>): unknown {
    const object = [...this.#objects.values()].find(
      (candidate) =>
        candidate.reference.export.instanceId === local.instanceId &&
        candidate.reference.export.name === local.name,
    );
    if (!object)
      throw new FrameError(
        "unavailable",
        "approved local provider has not prepared its export",
      );
    return object.value;
  }

  hasPendingAcquisition(operationId: string): boolean {
    const admission = this.#admissions.get(operationId);
    return !!admission && !admission.use && !admission.lifetime.closing;
  }

  async acquire(received: AcquireRequest): Promise<Use> {
    this.#assertConnected();
    const request = captureProtocol("AcquireRequest", received);
    const entry = this.#useExecution(request.execution);
    const previous = this.#admissions.get(request.operationId);
    const admission = this.#admission(request, entry);
    const revision = this.#connectionRevision;
    const channel = this.#channel;
    if (previous && !previous.hasRequest) {
      const grant = captureProtocol(
        "UseGrant",
        await admission.lifetime.run(() =>
          channel.observe((control, signal) =>
            control.acquire(request, signal),
          ),
        ),
      );
      admission.verifyGrant(grant);
      admission.bindRequest(request);
      const use = await admission.resolve(channel, () =>
        this.#materializeGrant(grant, entry, admission, channel, revision),
      );
      admission.verifyGrant(grant);
      return use;
    }
    admission.bindRequest(request);
    return admission.resolve(channel, async () => {
      const grant = await admission.requestGrant(channel);
      return this.#materializeGrant(grant, entry, admission, channel, revision);
    });
  }

  async accept(received: UseGrant): Promise<Use> {
    this.#assertConnected();
    const grant = captureProtocol("UseGrant", received);
    const original = this.#admissions.get(grant.operationId);
    original?.verifyGrant(grant);
    if (original?.use) return original.use;
    const entry = this.#useExecution(grant.execution);
    const admission = this.#admission(grant, entry);
    admission.verifyGrant(grant);
    const revision = this.#connectionRevision;
    const channel = this.#channel;
    const use = await admission.resolve(channel, () =>
      this.#materializeGrant(grant, entry, admission, channel, revision),
    );
    admission.verifyGrant(grant);
    return use;
  }

  #useExecution(execution: Execution): ExecutionResources {
    const entry = this.#executions.get(execution.id);
    if (!entry || !sameExecution(entry.plan.execution, execution))
      throw new FrameError("stale_binding", "use requires an owned execution");
    return entry;
  }

  #admission(
    identity: Pick<AcquireRequest, "operationId" | "parentUseId" | "execution">,
    entry: ExecutionResources,
  ): UseAdmission {
    if (!identity.operationId)
      throw new FrameError(
        "invalid_plan",
        "use operation identity is required",
      );
    const original = this.#admissions.get(identity.operationId);
    if (original) {
      original.verifyIdentity(identity);
      return original;
    }
    let parent = entry.lifetime;
    if (identity.parentUseId) {
      const delegated = this.#uses.get(identity.parentUseId);
      if (
        !delegated?.grant ||
        !sameExecution(delegated.grant.execution, identity.execution)
      )
        throw new FrameError(
          "stale_binding",
          "delegated use requires its original local parent",
        );
      parent = delegated.lifetime;
    }
    const reservation = `admission:${identity.operationId}`;
    this.#outbox.reserve(reservation);
    let lifetime: Lifetime;
    try {
      lifetime = parent.branch(reservation);
    } catch (error) {
      this.#outbox.cancelReservation(reservation);
      throw error;
    }
    const admission = new UseAdmission(identity, lifetime, reservation);
    this.#admissions.set(identity.operationId, admission);
    lifetime.observeExit((outcome) => {
      if (outcome.status !== "released" || admission.grant) return;
      // No callable object was materialized. Its original parent or execution
      // proof covers any Host acceptance whose acknowledgement remained unknown.
      this.#outbox.cancelReservation(reservation);
      this.#admissions.delete(identity.operationId);
    });
    return admission;
  }

  async #materializeGrant(
    received: UseGrant,
    entry: ExecutionResources,
    admission: UseAdmission,
    channel: ControlChannel,
    revision: number,
  ): Promise<Use> {
    const grant = captureProtocol("UseGrant", received);
    admission.verifyGrant(grant);
    if (
      !sameExecution(entry.plan.execution, grant.execution) ||
      !sameAuthority(entry.plan.authority, grant.authority) ||
      !sameRealm(grant.realm, this.#realm) ||
      grant.scopeId !== entry.plan.scopeId ||
      grant.scopeGeneration !== entry.plan.scopeGeneration
    )
      throw new FrameError(
        "permission_denied",
        "use grant does not match this approved execution",
      );
    // A durable acceptance receipt survives release. Only current authority may
    // admit a new local materialization after its terminal record was discarded.
    await channel.observe((control, signal) =>
      control.validateUse(grant, signal),
    );
    const existing = this.#uses.get(grant.id);
    if (existing && existing !== admission)
      throw new FrameError(
        "invalid_plan",
        "a different admission already owns this use",
      );
    const owner = admission.lifetime;
    const closing = revision !== this.#connectionRevision || owner.closing;
    const objects = new Map<string, ObjectResource>();
    const use = useView(grant, owner, objects, () => this.#assertConnected());
    admission.materialize(grant, use);
    this.#uses.set(grant.id, admission);
    this.#observeUseExit(entry, owner, admission.reservation, grant);
    owner.observeExit((outcome) => {
      if (outcome.status === "released") objects.clear();
    });
    try {
      if (closing)
        throw new FrameError(
          "unavailable",
          "use admission arrived after its owner stopped accepting work",
        );
      for (const reference of grant.objects) {
        const value = this.#objects.get(reference.objectId);
        if (!value || !sameObject(value.reference, reference))
          throw new FrameError(
            "stale_binding",
            "use grant does not name an exact prepared object",
          );
        value.lifetime.assertOpen();
        owner.dependsOn(value.lifetime);
        objects.set(reference.objectId, value);
      }
    } catch (error) {
      owner.beginClose();
      throw error;
    }
    return use;
  }

  async publish<M, V>(
    execution: Execution,
    instanceId: string,
    point: Point<M, V>,
    key: string,
    metadata: M,
    value: V,
  ): Promise<PublicationHandle> {
    this.#assertConnected();
    const entry = this.#executions.get(execution.id);
    if (!entry || !sameExecution(entry.plan.execution, execution))
      throw new FrameError(
        "stale_binding",
        "publication execution is unavailable",
      );
    return Publication.start(
      entry,
      { instanceId, point, key, metadata, value },
      this.#outbox,
      () => this.#publicationChannel(),
    );
  }

  #publicationChannel(): PublicationChannel {
    if (this.#connection !== "cleanupOnly") this.#assertConnected();
    return {
      channel: this.#channel,
      mode: this.#connection === "cleanupOnly" ? "cleanupOnly" : "connected",
    };
  }

  async route<T>(
    received: RouteGrant,
    event: Event<T>,
    listener: (payload: T, signal: AbortSignal) => Awaitable<void>,
  ): Promise<EventRoute<T>> {
    this.#assertConnected();
    const grant = captureProtocol("RouteGrant", received);
    if (
      event.ref.mode !== "native" ||
      !sameContract(event.ref, grant.source.contract) ||
      !sameContract(event.ref, grant.receiver.contract)
    )
      throw new FrameError(
        "contract_mismatch",
        "local event route requires one exact native event contract",
      );
    const revision = this.#connectionRevision;
    await this.#channel.observe((control, signal) =>
      control.validateRoute(grant, signal),
    );
    this.#assertRevision(revision);
    const use = await this.accept(grant.use);
    const instanceIds = new Set(
      grant.use.objects.map((object) => object.export.instanceId),
    );
    if (
      !instanceIds.has(grant.source.instanceId) ||
      !instanceIds.has(grant.receiver.instanceId)
    ) {
      await use.close();
      throw new FrameError(
        "permission_denied",
        "event route exceeds its approved use",
      );
    }
    return eventRouteView(use, {
      listener,
      admit: () => this.#assertConnected(),
    });
  }

  #observeUseExit(
    entry: ExecutionResources,
    lifetime: Lifetime,
    reservation: string,
    grant: UseGrant,
  ): void {
    lifetime.observeExit((outcome) => {
      if (outcome.status !== "released") return;
      this.#outbox.record(
        reservation,
        captureProtocol("CloseProof", {
          closeId: outcome.closeId,
          proofId: crypto.randomUUID(),
          authority: entry.plan.authority,
          realm: entry.plan.realm,
          execution: entry.plan.execution,
          scopeId: entry.plan.scopeId,
          scopeGeneration: entry.plan.scopeGeneration,
          scopeKind: "use",
          useId: grant.id,
          instances: [
            ...new Set(
              grant.objects.map((reference) => reference.export.instanceId),
            ),
          ].sort(),
          status: "released",
        }),
      );
    });
  }

  #forgetExecution(entry: ExecutionResources): void {
    if (this.#executions.get(entry.plan.execution.id) !== entry) return;
    this.#executions.delete(entry.plan.execution.id);
    // Poll creates no execution obligation. Invalidating its older projection
    // avoids retaining an unbounded history of acknowledged execution IDs.
    this.#executionProjectionRevision++;
  }

  disconnect(): void {
    if (this.#connection === "connected") this.#connection = "paused";
    this.#connectionRevision++;
    this.#channel.retire();
    this.#revalidation?.stopping.abort(SUPERSEDED_REVALIDATION);
  }

  async reconnect(control: HostControl): Promise<void> {
    await this.#revalidate(control);
    await this.flushProofs();
  }

  #revalidate(control: HostControl): Promise<void> {
    this.disconnect();
    const revision = this.#connectionRevision;
    const completion = Promise.withResolvers<void>();
    const work: Promise<void> = completion.promise.finally(() => {
      if (this.#revalidation?.work === work) this.#revalidation = undefined;
    });
    // Publish the original observation before the adapter can reenter polling.
    const stopping = new AbortController();
    this.#revalidation = { revision, work, stopping };
    this.#revalidateControl(control, revision, stopping.signal).then(
      completion.resolve,
      completion.reject,
    );
    return work;
  }

  async #revalidateControl(
    control: HostControl,
    revision: number,
    signal: AbortSignal,
  ): Promise<void> {
    const closing = this.#connection === "closed";
    const result = captureProtocol(
      "ReconnectResult",
      await observePromise(
        control.reconnect(
          {
            authority: this.#authority,
            realm: this.#realm,
            executions: [...this.#executions.values()]
              .filter(
                (entry) => entry.lifetime.exit?.outcome.status !== "released",
              )
              .map((entry) => entry.plan.execution),
          },
          signal,
        ),
        signal,
      ),
    );
    if (revision !== this.#connectionRevision)
      throw new FrameError("unavailable", "reconnection was superseded");
    if (!sameRealm(result.realm, this.#realm))
      throw new FrameError(
        "permission_denied",
        "reconnection belongs to another realm",
      );
    const sameEpoch = sameAuthority(result.authority, this.#authority);
    this.#channel = new ControlChannel(result.authority, control);
    if (closing) {
      this.#connection = "closed";
    } else if (sameEpoch && result.mode === "revalidated") {
      for (const entry of this.#executions.values()) {
        if (
          !result.executions.some(
            (approved) =>
              sameExecution(approved.execution, entry.plan.execution) &&
              approved.scopeId === entry.plan.scopeId &&
              approved.scopeGeneration === entry.plan.scopeGeneration,
          )
        )
          entry.lifetime.beginClose();
      }
      this.#connection = "connected";
    } else {
      this.#connection = "cleanupOnly";
      for (const entry of this.#executions.values())
        entry.lifetime.beginClose();
    }
  }

  /** Scheduling stays in this observation's turn so an ACK cannot race its reservation. */
  async poll(schedule: (plans: readonly LocalPlan[]) => void): Promise<void> {
    if (this.#connection === "closed") return;
    let revision = this.#connectionRevision;
    let plans: LocalPlan[];
    try {
      if (this.#connection === "paused") {
        const pending = this.#revalidation;
        const revalidating =
          pending?.revision === this.#connectionRevision
            ? pending.work
            : this.#revalidate(this.control);
        revision = this.#connectionRevision;
        await revalidating;
      }
      if (revision !== this.#connectionRevision) return;
      const executionProjectionRevision = this.#executionProjectionRevision;
      const pending = captureProtocol(
        "Poll",
        await this.#channel.observe((control, signal) => control.poll(signal)),
      );
      // A replacement channel owns current admission. A late observation from
      // the retired channel cannot fence it or turn it back into a paused one.
      if (revision !== this.#connectionRevision) return;
      this.#checkPollOwner(pending);
      for (const fence of pending.fences) {
        if (revision !== this.#connectionRevision) return;
        this.fence(fence.scopeId, fence.scopeGeneration);
      }
      if (revision !== this.#connectionRevision) return;
      plans = this.#schedulablePlans(pending, executionProjectionRevision);
    } catch (error) {
      if (revision !== this.#connectionRevision) return;
      this.disconnect();
      throw error;
    }
    schedule(plans);
  }

  #checkPollOwner(pending: Poll): void {
    if (!sameRealm(pending.realm, this.#realm))
      throw new FrameError(
        "permission_denied",
        "pending plans belong to another realm",
      );
    if (!sameAuthority(pending.authority, this.#authority))
      throw new FrameError(
        "unavailable",
        "pending plans require authority revalidation",
      );
  }

  // Stale observations cannot create executions, but already reserved admissions remain owned.
  #schedulablePlans(pending: Poll, projectionRevision: number): LocalPlan[] {
    const plans =
      this.#connection === "connected" &&
      projectionRevision === this.#executionProjectionRevision
        ? pending.plans.filter(
            (plan) =>
              this.#admissible(plan) &&
              !pending.fences.some(
                (fence) =>
                  fence.scopeId === plan.scopeId &&
                  fence.scopeGeneration === plan.scopeGeneration,
              ),
          )
        : [];
    const observed = new Set(plans.map((plan) => plan.execution.id));
    for (const entry of this.#executions.values())
      if (entry.admission.pending && !observed.has(entry.plan.execution.id))
        plans.push(entry.plan);
    return plans;
  }

  #admissible(plan: LocalPlan): boolean {
    const entry = this.#executions.get(plan.execution.id);
    return !entry || entry.admission.pending || !entry.lifetime.closing;
  }

  fence(scopeId: string, scopeGeneration: string): void {
    for (const entry of this.#executions.values()) {
      if (
        entry.plan.scopeId === scopeId &&
        entry.plan.scopeGeneration === scopeGeneration
      )
        entry.lifetime.beginClose();
    }
  }

  async flushProofs(): Promise<void> {
    try {
      await this.#outbox.flush(this.#channel, (proof) => {
        if (proof.scopeKind !== "execution") return;
        const entry = this.#executions.get(proof.execution.id);
        if (entry && sameExecution(entry.plan.execution, proof.execution))
          this.#forgetExecution(entry);
      });
    } finally {
      for (const [operationId, admission] of this.#admissions) {
        if (
          admission.lifetime.exit?.outcome.status !== "released" ||
          this.#outbox.reserved(admission.reservation)
        )
          continue;
        this.#admissions.delete(operationId);
        if (admission.grant) this.#uses.delete(admission.grant.id);
      }
    }
  }

  close(signal?: AbortSignal): Promise<readonly CloseOutcome[]> {
    if (!this.#closing) {
      const completion = Promise.withResolvers<readonly CloseOutcome[]>();
      const work = completion.promise.finally(() => {
        if (this.#closing === work) this.#closing = undefined;
      });
      this.#closing = work;
      this.#close().then(completion.resolve, completion.reject);
    }
    return observePromise(this.#closing, signal);
  }

  async #close(): Promise<readonly CloseOutcome[]> {
    this.#connection = "closed";
    this.#connectionRevision++;
    this.#channel.retire();
    this.#revalidation?.stopping.abort(SUPERSEDED_REVALIDATION);
    this.#channel = new ControlChannel(this.#authority, this.control);
    const entries = [...this.#executions.values()];
    const unresolved = new Set(
      entries.filter((entry) => entry.admission.pending),
    );
    const exits = entries.map((entry) => entry.lifetime.beginClose());
    await Promise.allSettled(
      entries.map((entry) => entry.admission.resolve(this.#channel)),
    );
    const outcomes = await Promise.all(
      exits.map((exit, index) => {
        const entry = entries[index]!;
        return unresolved.has(entry) && !entry.admission.pending
          ? exit.wait()
          : exit.settled();
      }),
    );
    await this.flushProofs();
    return Object.freeze(outcomes);
  }

  async reload(reload: () => void, signal?: AbortSignal): Promise<void> {
    const outcomes = await this.close(signal);
    if (
      outcomes.some((outcome) => outcome.status !== "released") ||
      !this.#outbox.empty
    )
      throw new FrameError(
        "close_unconfirmed",
        "reload requires complete release and durable proof acknowledgement",
      );
    reload();
  }
}
