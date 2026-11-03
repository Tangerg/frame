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
import { cloneJSON, parseJSON, stringifyJSON } from "../json.js";
import type {
  AcquireRequest,
  Authority,
  CloseProof,
  Execution,
  Import,
  LocalPlan,
  ObjectRef,
  PlanAdmissionDecision,
  PlannedUnit,
  PreparedContribution,
  PublicationRequest,
  PublicationResult,
  Realm,
  RouteGrant,
  UseGrant,
} from "../protocol.generated.js";
import { PROTOCOL } from "../protocol.generated.js";
import {
  isControlRefusal,
  type HostControl,
  sameAuthority,
} from "./control.js";
import { ProofOutbox } from "./outbox.js";
import { UseAdmission } from "./admission.js";
import { PreparationAdmission, ReadySubmission } from "./preparation.js";

export interface RuntimeOptions {
  readonly authority: Authority;
  readonly realm: Realm;
  readonly control: HostControl;
  readonly proofCapacity: number;
  load(unit: PlannedUnit, lifetime: Lifetime, plan: LocalPlan): Awaitable<Unit>;
  remote(binding: Import, lifetime: Lifetime): Awaitable<unknown>;
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

export interface PublicationHandle {
  readonly operationId: string;
  readonly object: ObjectRef;
  readonly outcome: PublicationResult;
  observe(): Promise<PublicationResult>;
  withdraw(): Promise<PublicationResult>;
}

export interface EventRoute<T> {
  dispatch(payload: T): Promise<void>;
  close(signal?: AbortSignal): Promise<CloseOutcome>;
}

interface PublicationState {
  readonly receipt: PublicationReceipt;
  observe?: () => Promise<PublicationResult>;
  withdraw?: () => Promise<PublicationResult>;
}

class PublicationReceipt {
  #outcome: PublicationResult = Object.freeze({ decision: "unknown" });
  #withdrawalConfirmed = false;

  constructor(
    readonly operationId: string,
    readonly object: ObjectRef,
  ) {}

  get outcome(): PublicationResult {
    return this.#outcome;
  }

  get acceptedPublication(): PublicationResult["publication"] {
    return this.#outcome.decision === "accepted"
      ? this.#outcome.publication
      : undefined;
  }

  receive(result: PublicationResult): PublicationResult {
    if (
      !result ||
      !["unknown", "accepted", "rejected"].includes(result.decision)
    )
      throw new FrameError("invalid_plan", "invalid publication decision");
    if (
      result.publication &&
      (!sameObject(result.publication.object, this.object) ||
        result.publication.operationId !== this.operationId)
    )
      throw new FrameError(
        "permission_denied",
        "publication response changed the original object or operation",
      );
    if (result.decision === "accepted" && !result.publication)
      throw new FrameError(
        "invalid_plan",
        "accepted publication has no exact publication identity",
      );
    const currentDecision = this.#outcome.decision;
    if (
      currentDecision !== "unknown" &&
      result.decision !== "unknown" &&
      result.decision !== currentDecision
    )
      throw new FrameError(
        "invalid_plan",
        "a confirmed publication decision cannot change",
      );
    const accepted = this.acceptedPublication;
    if (accepted) {
      if (
        result.publication &&
        (result.publication.id !== accepted.id ||
          result.publication.revision !== accepted.revision)
      )
        throw new FrameError(
          "permission_denied",
          "publication response changed the original publication identity",
        );
    }
    if (result.decision !== "unknown" || currentDecision === "unknown")
      this.#outcome = immutable(result);
    return this.#outcome;
  }

  uncertain(cause: unknown): void {
    if (this.#outcome.decision !== "unknown") return;
    this.#outcome = Object.freeze({
      decision: "unknown",
      problem:
        cause instanceof FrameError
          ? cause.message
          : "invalid publication acknowledgement",
    });
  }

  acknowledgeWithdrawal(): void {
    if (this.#outcome.decision !== "accepted")
      throw new FrameError(
        "invalid_plan",
        "publication must be accepted before withdrawal",
      );
    this.#withdrawalConfirmed = true;
  }

  get withdrawnPublicationId(): string | undefined {
    return this.#withdrawalConfirmed ? this.acceptedPublication?.id : undefined;
  }
}

function publicationView(state: PublicationState): PublicationHandle {
  return Object.freeze({
    operationId: state.receipt.operationId,
    object: state.receipt.object,
    get outcome() {
      return state.receipt.outcome;
    },
    observe: () =>
      state.observe ? state.observe() : Promise.resolve(state.receipt.outcome),
    withdraw: () =>
      state.withdraw
        ? state.withdraw()
        : Promise.reject(
            new FrameError("closed", "publication resource has exited"),
          ),
  });
}

// Public handles capture only their original resource state. Constructing them
// inside a Runtime method would share its lexical context with bookkeeping
// callbacks and could retain the Runtime after every resource has exited.
function executionView(
  identity: Execution,
  objects: ReadonlyMap<string, ObjectEntry>,
  lifetime: Lifetime,
): ExecutionHandle {
  return Object.freeze({
    identity,
    get objects() {
      return Object.freeze(
        [...objects.values()].map((entry) => entry.reference),
      );
    },
    close: (signal?: AbortSignal) => lifetime.close(signal),
    wait: (signal?: AbortSignal) => lifetime.wait(signal),
  });
}

function useView(
  grant: UseGrant,
  owner: Lifetime,
  objects: ReadonlyMap<string, ObjectEntry>,
): Use {
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
      const object = objects.get(objectKey(reference));
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

interface ObjectEntry {
  readonly reference: ObjectRef;
  readonly lifetime: Lifetime;
  value: unknown;
}

interface UnitEntry {
  readonly unit: PlannedUnit;
  readonly lifetime: Lifetime;
}

interface ExecutionEntry {
  readonly plan: LocalPlan;
  readonly fingerprint: string;
  readonly lifetime: Lifetime;
  readonly admission: PreparationAdmission;
  readonly ordered: readonly PlannedUnit[];
  readonly units: Map<string, UnitEntry>;
  readonly objects: Map<string, ObjectEntry>;
  readonly preparations: PreparedContribution[];
  readonly view: ExecutionHandle;
  prepared?: Promise<ExecutionHandle>;
  ready?: ReadySubmission;
}

function immutable<T>(value: T): T {
  const copy = cloneJSON(value);
  const freeze = (item: unknown): void => {
    if (item === null || typeof item !== "object" || Object.isFrozen(item))
      return;
    for (const child of Object.values(item)) freeze(child);
    Object.freeze(item);
  };
  freeze(copy);
  return copy;
}

function sameRealm(left: Realm, right: Realm): boolean {
  return left.id === right.id && left.epoch === right.epoch;
}
function sameExecution(left: Execution, right: Execution): boolean {
  return left.id === right.id && left.generation === right.generation;
}
function objectKey(reference: ObjectRef): string {
  return reference.objectId;
}
function sameObject(left: ObjectRef, right: ObjectRef): boolean {
  return (
    left.objectId === right.objectId &&
    left.export.instanceId === right.export.instanceId &&
    left.export.name === right.export.name &&
    sameContract(left.export.contract, right.export.contract)
  );
}

function orderUnits(
  plan: LocalPlan,
  external: ReadonlyMap<string, UnitEntry>,
): readonly PlannedUnit[] {
  const byId = new Map(plan.units.map((unit) => [unit.instance.id, unit]));
  if (!plan.units.length || byId.size !== plan.units.length)
    throw new FrameError(
      "invalid_plan",
      "execution must contain unique instances",
    );
  const visiting = new Set<string>();
  const visited = new Set<string>();
  const ordered: PlannedUnit[] = [];
  const visit = (unit: PlannedUnit): void => {
    const id = unit.instance.id;
    if (visiting.has(id))
      throw new FrameError(
        "invalid_plan",
        "approved execution has a dependency cycle",
      );
    if (visited.has(id)) return;
    visiting.add(id);
    const slots = new Set<string>();
    for (const binding of unit.imports) {
      if (slots.has(binding.slot))
        throw new FrameError("invalid_plan", "import slot was approved twice");
      slots.add(binding.slot);
      if (binding.kind === "local") {
        if (
          !binding.local ||
          binding.remote ||
          !sameContract(binding.contract, binding.local.contract)
        )
          throw new FrameError(
            "contract_mismatch",
            "local import differs from its approved contract",
          );
        const provider =
          byId.get(binding.local.instanceId) ??
          external.get(binding.local.instanceId)?.unit;
        if (
          !provider ||
          !provider.exports.some(
            (entry) =>
              entry.name === binding.local!.name &&
              sameContract(entry.contract, binding.contract),
          )
        )
          throw new FrameError(
            "invalid_plan",
            "local import does not identify an exact planned export",
          );
        if (byId.has(provider.instance.id)) visit(provider);
      } else if (binding.kind === "remote") {
        if (
          !binding.remote ||
          binding.local ||
          binding.contract.mode !== "portable" ||
          !sameContract(binding.remote.source.contract, binding.contract)
        )
          throw new FrameError(
            "contract_mismatch",
            "remote import requires an exact portable binding",
          );
      } else if (binding.kind !== "absent" || binding.local || binding.remote) {
        throw new FrameError("invalid_plan", "invalid import kind");
      }
    }
    visiting.delete(id);
    visited.add(id);
    ordered.push(unit);
  };
  for (const unit of plan.units) visit(unit);
  return ordered;
}

/** Executes approved plans. Installation, discovery, and selection stay in Go. */
export class Runtime {
  readonly #realm: Realm;
  #authority: Authority;
  #control: HostControl;
  readonly #load: RuntimeOptions["load"];
  readonly #remote: RuntimeOptions["remote"];
  readonly #outbox: ProofOutbox;
  #connection: "connected" | "paused" | "cleanupOnly" | "closed" = "connected";
  #connectionRevision = 0;
  #executionProjectionRevision = 0;
  #executions = new Map<string, ExecutionEntry>();
  #instances = new Map<string, UnitEntry>();
  #objects = new Map<string, ObjectEntry>();
  #uses = new Map<string, UseAdmission>();
  #admissions = new Map<string, UseAdmission>();

  constructor(options: RuntimeOptions) {
    this.#realm = immutable(options.realm);
    this.#authority = immutable(options.authority);
    this.#control = options.control;
    this.#load = options.load;
    this.#remote = options.remote;
    this.#outbox = new ProofOutbox(options.proofCapacity);
  }

  get authority(): Authority {
    return this.#authority;
  }
  get realm(): Realm {
    return this.#realm;
  }
  get connection(): "connected" | "paused" | "cleanupOnly" | "closed" {
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
    const plan = immutable(received);
    const fingerprint = stringifyJSON(plan, true);
    const existing = this.#executions.get(plan.execution.id);
    if (existing) {
      if (existing.fingerprint !== fingerprint)
        throw new FrameError(
          "stale_binding",
          "execution identity cannot be relabeled or restarted",
        );
      if (existing.prepared) return existing.prepared;
      if (existing.admission.pending)
        return this.#resumePreparation(existing, this.#connectionRevision);
      if (existing.lifetime.closing)
        throw new FrameError("stale_binding", "original execution is closing");
      this.#assertConnected();
      if (!existing.ready)
        throw new FrameError(
          "invalid_plan",
          "execution has no prepared report",
        );
      return this.#resumePreparation(existing, this.#connectionRevision);
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
    const ordered = orderUnits(plan, this.#instances);
    if (ordered.some((unit) => this.#instances.has(unit.instance.id)))
      throw new FrameError(
        "invalid_plan",
        "a different execution already owns this instance",
      );
    this.#outbox.reserve(`execution:${plan.execution.id}`);
    const lifetime = new Lifetime(`execution:${plan.execution.id}`);
    const objects = new Map<string, ObjectEntry>();
    const units = new Map<string, UnitEntry>();
    const view = executionView(plan.execution, objects, lifetime);
    const admission = new PreparationAdmission(plan, lifetime, (decision) =>
      this.#settlePreparation(entry, decision),
    );
    const entry: ExecutionEntry = {
      plan,
      fingerprint,
      lifetime,
      admission,
      ordered,
      units,
      objects,
      preparations: [],
      view,
    };
    this.#executions.set(plan.execution.id, entry);
    return this.#resumePreparation(entry, this.#connectionRevision);
  }

  #resumePreparation(
    entry: ExecutionEntry,
    revision: number,
  ): Promise<ExecutionHandle> {
    const work = entry.ready
      ? this.#submitReady(entry, revision)
      : this.#prepare(entry, entry.ordered, revision);
    entry.prepared = work.finally(() => {
      entry.prepared = undefined;
    });
    return entry.prepared;
  }

  #settlePreparation(
    entry: ExecutionEntry,
    decision: PlanAdmissionDecision,
  ): void {
    const reservation = `execution:${entry.plan.execution.id}`;
    switch (decision) {
      case "notAdmitted":
      case "settled":
        this.#outbox.cancelReservation(reservation);
        this.#forgetExecution(entry);
        entry.lifetime.beginClose();
        return;
      case "admitted":
      case "cleanupRequired":
        this.#observeExit(
          entry,
          entry.lifetime,
          reservation,
          "execution",
          entry.plan.execution.id,
          entry.ordered.map((unit) => unit.instance.id),
        );
        if (decision === "cleanupRequired") entry.lifetime.beginClose();
        return;
      default:
        throw new FrameError(
          "outcome_unknown",
          "host has not settled the original preparation admission",
        );
    }
  }

  async #prepare(
    entry: ExecutionEntry,
    ordered: readonly PlannedUnit[],
    revision: number,
  ): Promise<ExecutionHandle> {
    const admit = () => entry.admission.resolve(this.#control, this.#authority);
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
      for (const unit of ordered) {
        const owned = {
          unit,
          lifetime: entry.lifetime.branch(`instance:${unit.instance.id}`),
        };
        entry.units.set(unit.instance.id, owned);
        this.#instances.set(unit.instance.id, owned);
      }
      for (const ownedUnit of entry.units.values()) {
        for (const binding of ownedUnit.unit.imports) {
          if (binding.kind === "local")
            ownedUnit.lifetime.dependsOn(
              this.#instances.get(binding.local!.instanceId)!.lifetime,
            );
        }
      }
      await entry.lifetime.run(async () => {
        for (const unit of ordered) {
          const owner = entry.units.get(unit.instance.id)!.lifetime;
          owner.assertOpen();
          this.#assertRevision(revision);
          const definition = await owner.run(() =>
            this.#load(unit, owner, entry.plan),
          );
          this.#assertRevision(revision);
          owner.assertOpen();
          if (definition.id !== unit.instance.source.unitId)
            throw new FrameError(
              "contract_mismatch",
              "module does not declare the approved unit",
            );
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
              const local = [...this.#objects.values()].find(
                (object) =>
                  object.reference.export.instanceId ===
                    binding.local!.instanceId &&
                  object.reference.export.name === binding.local!.name,
              );
              if (!local)
                throw new FrameError(
                  "unavailable",
                  "approved local provider has not prepared its export",
                );
              imports[binding.slot] = local.value;
            } else {
              imports[binding.slot] = await owner.run(() =>
                this.#remote(binding, owner),
              );
              this.#assertRevision(revision);
              owner.assertOpen();
            }
          }
          if (Object.keys(definition.exports).length !== unit.exports.length)
            throw new FrameError(
              "contract_mismatch",
              "module exports differ from the approved declaration",
            );
          for (const declared of unit.exports) {
            if (
              !Object.hasOwn(definition.exports, declared.name) ||
              !definition.exports[declared.name] ||
              !sameContract(
                definition.exports[declared.name]!.ref,
                declared.contract,
              )
            )
              throw new FrameError(
                "contract_mismatch",
                "module exports an unapproved contract",
              );
          }
          const context: Setup<typeof definition.imports> = Object.freeze({
            imports: Object.freeze(imports),
            configuration: parseJSON(unit.configurationJson),
            lifetime: owner,
          });
          this.#assertRevision(revision);
          const exports = await owner.run(() => definition.setup(context));
          this.#assertRevision(revision);
          owner.assertOpen();
          if (!exports || Object.keys(exports).length !== unit.exports.length)
            throw new FrameError(
              "contract_mismatch",
              "actual export set differs from its declaration",
            );
          for (const declared of unit.exports) {
            if (!Object.hasOwn(exports, declared.name))
              throw new FrameError(
                "contract_mismatch",
                "declared export was not prepared",
              );
            const exported = exports[declared.name];
            if (declared.contract.kind === "point") {
              if (
                typeof exported !== "object" ||
                exported === null ||
                !Object.hasOwn(exported, "metadata") ||
                !Object.hasOwn(exported, "value")
              )
                throw new FrameError(
                  "contract_mismatch",
                  "point export requires metadata and a value",
                );
              const { metadata, value } = exported as {
                metadata: unknown;
                value: unknown;
              };
              const object = this.#stage(
                entry,
                unit.instance.id,
                definition.exports[declared.name] as Point<unknown, unknown>,
                declared.name,
                value,
                owner,
              );
              entry.preparations.push(
                immutable({
                  object: object.reference,
                  metadataJson: stringifyJSON(metadata, true),
                }),
              );
              continue;
            }
            if (declared.contract.kind === "service" && exported == null)
              throw new FrameError(
                "contract_mismatch",
                "service implementation is missing",
              );
            const reference: ObjectRef = immutable({
              export: {
                instanceId: unit.instance.id,
                name: declared.name,
                contract: declared.contract,
              },
              objectId: crypto.randomUUID(),
            });
            const object: ObjectEntry = {
              reference,
              value: exports[declared.name],
              lifetime: owner,
            };
            entry.objects.set(objectKey(reference), object);
            this.#objects.set(objectKey(reference), object);
          }
        }
      });
      this.#assertRevision(revision);
    } catch (error) {
      await entry.lifetime.close();
      throw error;
    }
    entry.lifetime.assertOpen();
    entry.ready = new ReadySubmission(
      immutable({
        operationId: crypto.randomUUID(),
        authority: entry.plan.authority,
        realm: this.#realm,
        execution: entry.plan.execution,
        objects: entry.view.objects,
        contributions: entry.preparations,
      }),
      entry.lifetime,
    );
    return this.#submitReady(entry, revision);
  }

  async #submitReady(
    entry: ExecutionEntry,
    revision: number,
  ): Promise<ExecutionHandle> {
    this.#assertRevision(revision);
    entry.lifetime.assertOpen();
    await entry.ready!.submit(this.#control);
    this.#assertRevision(revision);
    entry.lifetime.assertOpen();
    return entry.view;
  }

  #stage<M, V>(
    entry: ExecutionEntry,
    instanceId: string,
    point: Point<M, V>,
    key: string,
    value: V,
    owner: Lifetime,
  ): ObjectEntry {
    owner.assertOpen();
    if (value == null)
      throw new FrameError(
        "contract_mismatch",
        "point implementation is missing",
      );
    const unit = entry.units.get(instanceId)?.unit;
    if (
      !unit ||
      !unit.exports.some((item) => sameContract(item.contract, point.ref))
    )
      throw new FrameError(
        "permission_denied",
        "unit did not declare this public contribution contract",
      );
    if (
      !key ||
      [...entry.objects.values()].some(
        (item) =>
          item.reference.export.instanceId === instanceId &&
          item.reference.export.name === key,
      )
    )
      throw new FrameError(
        "invalid_plan",
        "contribution key is empty or already prepared",
      );
    const reference: ObjectRef = immutable({
      export: { instanceId, name: key, contract: point.ref },
      objectId: crypto.randomUUID(),
    });
    const object: ObjectEntry = { reference, lifetime: owner, value };
    entry.objects.set(objectKey(reference), object);
    this.#objects.set(objectKey(reference), object);
    return object;
  }

  hasPendingAcquisition(operationId: string): boolean {
    const admission = this.#admissions.get(operationId);
    return !!admission && !admission.use && !admission.lifetime.closing;
  }

  async acquire(received: AcquireRequest): Promise<Use> {
    this.#assertConnected();
    const request = immutable(received);
    const entry = this.#useExecution(request.execution);
    const previous = this.#admissions.get(request.operationId);
    const admission = this.#admission(request, entry);
    const revision = this.#connectionRevision;
    if (previous && !previous.hasRequest) {
      const grant = await admission.lifetime.run(() =>
        this.#control.acquire(request),
      );
      admission.verifyGrant(grant);
      admission.bindRequest(request);
      const use = await admission.resolve(() =>
        this.#materializeGrant(grant, entry, admission, revision),
      );
      admission.verifyGrant(grant);
      return use;
    }
    admission.bindRequest(request);
    return admission.resolve(async () => {
      const grant = await admission.requestGrant(this.#control);
      return this.#materializeGrant(grant, entry, admission, revision);
    });
  }

  async accept(received: UseGrant): Promise<Use> {
    this.#assertConnected();
    const grant = immutable(received);
    const original = this.#admissions.get(grant.operationId);
    original?.verifyGrant(grant);
    if (original?.use) return original.use;
    const entry = this.#useExecution(grant.execution);
    const admission = this.#admission(grant, entry);
    admission.verifyGrant(grant);
    const revision = this.#connectionRevision;
    const use = await admission.resolve(() =>
      this.#materializeGrant(grant, entry, admission, revision),
    );
    admission.verifyGrant(grant);
    return use;
  }

  #useExecution(execution: Execution): ExecutionEntry {
    const entry = this.#executions.get(execution.id);
    if (!entry || !sameExecution(entry.plan.execution, execution))
      throw new FrameError("stale_binding", "use requires an owned execution");
    return entry;
  }

  #admission(
    identity: Pick<AcquireRequest, "operationId" | "parentUseId" | "execution">,
    entry: ExecutionEntry,
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
    entry: ExecutionEntry,
    admission: UseAdmission,
    revision: number,
  ): Promise<Use> {
    const grant = immutable(received);
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
    await this.#control.validateUse(grant);
    const existing = this.#uses.get(grant.id);
    if (existing && existing !== admission)
      throw new FrameError(
        "invalid_plan",
        "a different admission already owns this use",
      );
    const owner = admission.lifetime;
    const closing = revision !== this.#connectionRevision || owner.closing;
    const objects = new Map<string, ObjectEntry>();
    const use = useView(grant, owner, objects);
    admission.materialize(grant, use);
    this.#uses.set(grant.id, admission);
    this.#observeExit(
      entry,
      owner,
      admission.reservation,
      "use",
      grant.id,
      [
        ...new Set(
          grant.objects.map((reference) => reference.export.instanceId),
        ),
      ].sort(),
    );
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
        const value = this.#objects.get(objectKey(reference));
        if (!value || !sameObject(value.reference, reference))
          throw new FrameError(
            "stale_binding",
            "use grant does not name an exact prepared object",
          );
        value.lifetime.assertOpen();
        owner.dependsOn(value.lifetime);
        objects.set(objectKey(reference), value);
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
    const unit = entry.units.get(instanceId);
    if (!unit)
      throw new FrameError(
        "permission_denied",
        "publication instance is outside this execution",
      );
    const operationId = crypto.randomUUID();
    const reservation = `publication:${operationId}`;
    const metadataJson = stringifyJSON(metadata, true);
    this.#outbox.reserve(reservation);
    let owner: Lifetime | undefined;
    let object: ObjectEntry;
    try {
      owner = unit.lifetime.branch(reservation);
      object = this.#stage(entry, instanceId, point, key, value, owner);
    } catch (error) {
      owner?.beginClose();
      this.#outbox.cancelReservation(reservation);
      throw error;
    }
    const request: PublicationRequest = immutable({
      operationId,
      authority: entry.plan.authority,
      realm: this.#realm,
      execution,
      object: object.reference,
      metadataJson,
    });
    const receipt = new PublicationReceipt(operationId, object.reference);
    const state: PublicationState = { receipt };
    const receive = (result: PublicationResult): PublicationResult => {
      const observed = receipt.receive(result);
      if (observed.decision === "rejected") owner.beginClose();
      return observed;
    };
    state.observe = async () => {
      if (this.#connection === "cleanupOnly") return receipt.outcome;
      this.#assertConnected();
      return receive(await this.#control.publication(operationId));
    };
    state.withdraw = async () => {
      this.#assertConnected();
      if (receipt.outcome.decision !== "accepted")
        receive(await this.#control.publication(operationId));
      const accepted = receipt.acceptedPublication;
      if (!accepted) return { decision: "unknown" };
      const result = await this.#control.withdraw({
        authority: this.#authority,
        publicationId: accepted.id,
      });
      if (result.decision === "accepted") {
        receipt.acknowledgeWithdrawal();
        await owner.close();
      }
      return result;
    };
    owner.observeExit((outcome) => {
      if (outcome.status !== "released") return;
      entry.objects.delete(objectKey(object.reference));
      this.#objects.delete(objectKey(object.reference));
      object.value = undefined;
      state.observe = undefined;
      state.withdraw = undefined;
      const publicationId = receipt.withdrawnPublicationId;
      if (publicationId) {
        this.#outbox.record(
          reservation,
          immutable({
            closeId: outcome.closeId,
            proofId: crypto.randomUUID(),
            authority: entry.plan.authority,
            realm: entry.plan.realm,
            execution: entry.plan.execution,
            scopeId: entry.plan.scopeId,
            scopeGeneration: entry.plan.scopeGeneration,
            scopeKind: "publication",
            publicationId,
            instances: [object.reference.export.instanceId],
            status: "released",
          }),
        );
      } else {
        // Only an acknowledged Host withdrawal has an independent publication
        // exit. Execution retirement covers its original objects, including an
        // accepted or uncertain publication that was not separately withdrawn.
        // The execution reservation remains until that whole exit is proven.
        this.#outbox.cancelReservation(reservation);
      }
    });
    const handle = publicationView(state);
    let result: PublicationResult;
    try {
      result = await owner.run(() => this.#control.publish(request));
    } catch (cause) {
      if (isControlRefusal(cause)) {
        await owner.close();
        throw cause;
      }
      // The original operation remains observable after an uncertain acknowledgement.
      return handle;
    }
    try {
      receive(result);
    } catch (cause) {
      // A malformed acknowledgement leaves the original operation observable.
      receipt.uncertain(cause);
    }
    if (receipt.outcome.decision === "rejected") await owner.close();
    return handle;
  }

  async route<T>(
    grant: RouteGrant,
    event: Event<T>,
    listener: (payload: T, signal: AbortSignal) => Awaitable<void>,
  ): Promise<EventRoute<T>> {
    this.#assertConnected();
    if (
      event.ref.mode !== "native" ||
      !sameContract(event.ref, grant.source.contract) ||
      !sameContract(event.ref, grant.receiver.contract)
    )
      throw new FrameError(
        "contract_mismatch",
        "local event route requires one exact native event contract",
      );
    await this.#control.validateRoute(grant);
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

  #observeExit(
    entry: ExecutionEntry,
    lifetime: Lifetime,
    reservation: string,
    targetKind: "execution" | "use",
    targetId: string,
    instances: readonly string[],
  ): void {
    lifetime.observeExit((outcome) => {
      if (outcome.status !== "released") return;
      const proof: CloseProof = immutable({
        closeId: outcome.closeId,
        proofId: crypto.randomUUID(),
        authority: entry.plan.authority,
        realm: entry.plan.realm,
        execution: entry.plan.execution,
        scopeId: entry.plan.scopeId,
        scopeGeneration: entry.plan.scopeGeneration,
        scopeKind: targetKind,
        ...(targetKind === "use" ? { useId: targetId } : {}),
        instances,
        status: "released",
      });
      this.#outbox.record(reservation, proof);
      if (targetKind === "execution") {
        for (const object of entry.objects.values()) {
          object.value = undefined;
          this.#objects.delete(objectKey(object.reference));
        }
        for (const instanceId of entry.units.keys())
          this.#instances.delete(instanceId);
        entry.objects.clear();
        entry.units.clear();
        entry.preparations.length = 0;
        entry.prepared = undefined;
        entry.ready = undefined;
      }
    });
  }

  #forgetExecution(entry: ExecutionEntry): void {
    if (this.#executions.get(entry.plan.execution.id) !== entry) return;
    this.#executions.delete(entry.plan.execution.id);
    // Poll creates no execution obligation. Invalidating its older projection
    // avoids retaining an unbounded history of acknowledged execution IDs.
    this.#executionProjectionRevision++;
  }

  disconnect(): void {
    if (this.#connection === "connected") this.#connection = "paused";
    this.#connectionRevision++;
  }

  async reconnect(control: HostControl): Promise<void> {
    await this.#revalidate(control);
    await this.flushProofs();
  }

  async #revalidate(control: HostControl): Promise<void> {
    const closing = this.#connection === "closed";
    this.disconnect();
    const revision = this.#connectionRevision;
    const result = await control.reconnect({
      authority: this.#authority,
      realm: this.#realm,
      executions: [...this.#executions.values()]
        .filter((entry) => entry.lifetime.exit?.outcome.status !== "released")
        .map((entry) => entry.plan.execution),
    });
    if (revision !== this.#connectionRevision)
      throw new FrameError("unavailable", "reconnection was superseded");
    if (!sameRealm(result.realm, this.#realm))
      throw new FrameError(
        "permission_denied",
        "reconnection belongs to another realm",
      );
    const sameEpoch = sameAuthority(result.authority, this.#authority);
    this.#authority = immutable(result.authority);
    this.#control = control;
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
        const revalidating = this.#revalidate(this.#control);
        revision = this.#connectionRevision;
        await revalidating;
      }
      if (revision !== this.#connectionRevision) return;
      const executionProjectionRevision = this.#executionProjectionRevision;
      const pending = await this.#control.poll();
      // A replacement channel owns current admission. A late observation from
      // the retired channel cannot fence it or turn it back into a paused one.
      if (revision !== this.#connectionRevision) return;
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
      for (const fence of pending.fences) {
        if (revision !== this.#connectionRevision) return;
        this.fence(fence.scopeId, fence.scopeGeneration);
      }
      if (revision !== this.#connectionRevision) return;
      plans =
        this.#connection === "connected" &&
        executionProjectionRevision === this.#executionProjectionRevision
          ? pending.plans.filter((plan) => {
              const entry = this.#executions.get(plan.execution.id);
              return (
                (!entry ||
                  entry.admission.pending ||
                  !entry.lifetime.closing) &&
                !pending.fences.some(
                  (fence) =>
                    fence.scopeId === plan.scopeId &&
                    fence.scopeGeneration === plan.scopeGeneration,
                )
              );
            })
          : [];
      const observed = new Set(plans.map((plan) => plan.execution.id));
      for (const entry of this.#executions.values())
        if (entry.admission.pending && !observed.has(entry.plan.execution.id))
          plans.push(entry.plan);
    } catch (error) {
      if (revision !== this.#connectionRevision) return;
      this.disconnect();
      throw error;
    }
    schedule(plans);
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
      await this.#outbox.flush(this.#control, this.#authority, (proof) => {
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

  async close(signal?: AbortSignal): Promise<readonly CloseOutcome[]> {
    this.#connection = "closed";
    this.#connectionRevision++;
    const entries = [...this.#executions.values()];
    const unresolved = new Set(
      entries.filter((entry) => entry.admission.pending),
    );
    const exits = entries.map((entry) => entry.lifetime.beginClose());
    await Promise.allSettled(
      entries.map((entry) =>
        entry.admission.resolve(this.#control, this.#authority, signal),
      ),
    );
    const outcomes = await Promise.all(
      exits.map((exit, index) => {
        const entry = entries[index]!;
        return unresolved.has(entry) && !entry.admission.pending
          ? exit.wait(signal)
          : exit.settled(signal);
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
