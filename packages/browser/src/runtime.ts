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
} from "@frame/sdk";
import { cloneJSON, parseJSON, stringifyJSON } from "@frame/sdk/json";
import type {
  AcquireRequest,
  Authority,
  CloseProof,
  Execution,
  Import,
  LocalPlan,
  ObjectRef,
  PlannedUnit,
  PreparedContribution,
  PublicationRequest,
  PublicationResult,
  Realm,
  ReadyReport,
  RouteGrant,
  UseGrant,
} from "@frame/sdk/protocol";
import { PROTOCOL } from "@frame/sdk/protocol";
import { type HostControl, sameAuthority } from "./control.js";
import { ProofOutbox } from "./outbox.js";

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
  readonly operationId: string;
  readonly object: ObjectRef;
  outcome: PublicationResult;
  observe?: () => Promise<PublicationResult>;
  withdraw?: () => Promise<PublicationResult>;
}

function publicationView(state: PublicationState): PublicationHandle {
  return Object.freeze({
    operationId: state.operationId,
    object: state.object,
    get outcome() {
      return state.outcome;
    },
    observe: () =>
      state.observe ? state.observe() : Promise.resolve(state.outcome),
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
  readonly units: Map<string, UnitEntry>;
  readonly objects: Map<string, ObjectEntry>;
  readonly preparations: PreparedContribution[];
  readonly view: ExecutionHandle;
  prepared?: Promise<ExecutionHandle>;
  ready?: ReadyReport;
}

interface AdmissionEntry {
  readonly request: AcquireRequest;
  readonly execution: ExecutionEntry;
  readonly lifetime: Lifetime;
  readonly reservation: string;
  pending?: Promise<Use>;
  use?: Use;
  granted?: boolean;
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
  #executions = new Map<string, ExecutionEntry>();
  #instances = new Map<string, UnitEntry>();
  #objects = new Map<string, ObjectEntry>();
  #uses = new Map<string, { readonly grant: UseGrant; readonly use: Use }>();
  #admissions = new Map<string, AdmissionEntry>();

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

  async prepare(received: LocalPlan): Promise<ExecutionHandle> {
    this.#assertConnected();
    const plan = immutable(received);
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
    const revision = this.#connectionRevision;
    await this.#control.validatePlan(plan);
    if (revision !== this.#connectionRevision)
      throw new FrameError(
        "unavailable",
        "plan validation arrived on a retired control channel",
      );
    this.#assertConnected();
    const ordered = orderUnits(plan, this.#instances);
    const fingerprint = stringifyJSON(plan, true);
    const existing = this.#executions.get(plan.execution.id);
    if (existing) {
      if (existing.fingerprint !== fingerprint || existing.lifetime.closing)
        throw new FrameError(
          "stale_binding",
          "execution identity cannot be relabeled or restarted",
        );
      if (existing.prepared) return existing.prepared;
      if (!existing.ready)
        throw new FrameError(
          "invalid_plan",
          "execution has no prepared report",
        );
      existing.prepared = this.#submitReady(existing).finally(() => {
        existing.prepared = undefined;
      });
      return existing.prepared;
    }
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
    const entry: ExecutionEntry = {
      plan,
      fingerprint,
      lifetime,
      units,
      objects,
      preparations: [],
      view,
    };
    this.#executions.set(plan.execution.id, entry);
    this.#observeExit(
      entry,
      lifetime,
      `execution:${plan.execution.id}`,
      "execution",
      plan.execution.id,
      ordered.map((unit) => unit.instance.id),
    );
    entry.prepared = this.#prepare(entry, ordered).finally(() => {
      entry.prepared = undefined;
    });
    return entry.prepared;
  }

  async #prepare(
    entry: ExecutionEntry,
    ordered: readonly PlannedUnit[],
  ): Promise<ExecutionHandle> {
    try {
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
          const definition = await this.#load(unit, owner, entry.plan);
          this.#assertConnected();
          owner.assertOpen();
          if (definition.id !== unit.instance.source.unitId)
            throw new FrameError(
              "contract_mismatch",
              "module does not declare the approved unit",
            );
          const imports: Record<string, unknown> = {};
          if (Object.keys(definition.imports).length !== unit.imports.length)
            throw new FrameError(
              "contract_mismatch",
              "module imports differ from the approved slots",
            );
          for (const binding of unit.imports) {
            const declared = definition.imports[binding.slot];
            if (!declared)
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
              imports[binding.slot] = await this.#remote(binding, owner);
            }
          }
          if (Object.keys(definition.exports).length !== unit.exports.length)
            throw new FrameError(
              "contract_mismatch",
              "module exports differ from the approved declaration",
            );
          for (const declared of unit.exports) {
            if (
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
          const exports = await owner.run(() => definition.setup(context));
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
              if (value == null)
                throw new FrameError(
                  "contract_mismatch",
                  "point implementation is missing",
                );
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
    } catch (error) {
      await entry.lifetime.close();
      throw error;
    }
    entry.lifetime.assertOpen();
    entry.ready = immutable({
      operationId: crypto.randomUUID(),
      authority: entry.plan.authority,
      realm: this.#realm,
      execution: entry.plan.execution,
      objects: entry.view.objects,
      contributions: entry.preparations,
    });
    return this.#submitReady(entry);
  }

  async #submitReady(entry: ExecutionEntry): Promise<ExecutionHandle> {
    entry.lifetime.assertOpen();
    let result: PublicationResult;
    try {
      result = await entry.lifetime.run(() =>
        this.#control.ready(entry.ready!),
      );
    } catch (cause) {
      // Publication may have committed. Prepared values remain owned and may be
      // referenced by an exact Host Use even though this acknowledgement is lost.
      throw new FrameError(
        "outcome_unknown",
        "ready acknowledgement is unknown; prepared execution is retained",
        { cause },
      );
    }
    if (result.decision === "rejected") {
      await entry.lifetime.close();
      throw new FrameError(
        "stale_binding",
        "host rejected the prepared execution",
      );
    }
    if (result.decision !== "accepted")
      throw new FrameError(
        "outcome_unknown",
        "prepared acknowledgement remains unknown",
      );
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

  async acquire(received: AcquireRequest): Promise<Use> {
    this.#assertConnected();
    const request = immutable(received);
    const entry = this.#executions.get(request.execution.id);
    if (!entry || !sameExecution(entry.plan.execution, request.execution))
      throw new FrameError(
        "stale_binding",
        "acquisition requires an owned execution",
      );
    let admission = this.#admissions.get(request.operationId);
    if (admission) {
      if (
        stringifyJSON(admission.request, true) !== stringifyJSON(request, true)
      )
        throw new FrameError(
          "invalid_plan",
          "admission operation cannot be relabeled",
        );
      if (admission.use) return admission.use;
      if (admission.pending) return admission.pending;
      admission.lifetime.assertOpen();
    } else {
      const reservation = `admission:${request.operationId}`;
      this.#outbox.reserve(reservation);
      let owner: Lifetime;
      try {
        owner = entry.lifetime.branch(reservation);
      } catch (error) {
        this.#outbox.cancelReservation(reservation);
        throw error;
      }
      admission = { request, execution: entry, lifetime: owner, reservation };
      this.#admissions.set(request.operationId, admission);
      const captured = admission;
      owner.observeExit((outcome) => {
        if (outcome.status !== "released") return;
        this.#admissions.delete(request.operationId);
        // No object was dispatched when an acknowledgement never arrived. The
        // execution proof settles any Host Use retained under this operation.
        if (!captured.granted) this.#outbox.cancelReservation(reservation);
      });
    }
    const captured = admission;
    const revision = this.#connectionRevision;
    admission.pending = admission.lifetime
      .run(async () => {
        const grant = await this.#control.acquire(captured.request);
        const closing =
          revision !== this.#connectionRevision || captured.lifetime.closing;
        const use = this.#attachGrant(
          grant,
          captured.execution,
          captured.lifetime,
          captured.reservation,
          closing,
          () => {
            captured.granted = true;
          },
        );
        captured.use = use;
        if (closing) {
          use.lifetime.beginClose();
          throw new FrameError(
            "unavailable",
            "admission arrived after its owner stopped accepting work",
          );
        }
        return use;
      })
      .finally(() => {
        captured.pending = undefined;
      });
    return admission.pending;
  }

  async accept(received: UseGrant): Promise<Use> {
    this.#assertConnected();
    const grant = immutable(received);
    const existing = this.#uses.get(grant.id);
    if (existing) {
      if (stringifyJSON(existing.grant, true) !== stringifyJSON(grant, true))
        throw new FrameError(
          "permission_denied",
          "use identity cannot be relabeled",
        );
      return existing.use;
    }
    const entry = this.#executions.get(grant.execution.id);
    if (!entry)
      throw new FrameError("stale_binding", "use execution is unavailable");
    const reservation = `use:${grant.id}`;
    this.#outbox.reserve(reservation);
    let owner: Lifetime;
    try {
      owner = entry.lifetime.branch(reservation);
    } catch (error) {
      this.#outbox.cancelReservation(reservation);
      throw error;
    }
    const revision = this.#connectionRevision;
    let admitted = false;
    try {
      const use = await owner.run(async () => {
        await this.#control.validateUse(grant);
        const closing = revision !== this.#connectionRevision || owner.closing;
        const use = this.#attachGrant(
          grant,
          entry,
          owner,
          reservation,
          closing,
          () => {
            admitted = true;
          },
        );
        if (closing) {
          owner.beginClose();
          throw new FrameError(
            "unavailable",
            "use validation arrived after its owner closed",
          );
        }
        return use;
      });
      return use;
    } catch (error) {
      await owner.close();
      if (!admitted) this.#outbox.cancelReservation(reservation);
      throw error;
    }
  }

  #attachGrant(
    received: UseGrant,
    entry: ExecutionEntry,
    owner: Lifetime,
    reservation: string,
    closing: boolean,
    markOwned: () => void,
  ): Use {
    const grant = immutable(received);
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
    const existing = this.#uses.get(grant.id);
    if (existing)
      throw new FrameError(
        "invalid_plan",
        "a different admission already owns this use",
      );
    const objects = new Map<string, ObjectEntry>();
    this.#observeExit(
      entry,
      owner,
      reservation,
      "use",
      grant.id,
      [
        ...new Set(
          grant.objects.map((reference) => reference.export.instanceId),
        ),
      ].sort(),
    );
    markOwned();
    try {
      if (!closing) {
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
      }
    } catch (error) {
      owner.beginClose();
      throw error;
    }
    const use = useView(grant, owner, objects);
    this.#uses.set(grant.id, { grant, use });
    owner.observeExit((outcome) => {
      if (outcome.status !== "released") return;
      objects.clear();
      this.#uses.delete(grant.id);
    });
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
    const state: PublicationState = {
      operationId,
      object: object.reference,
      outcome: Object.freeze({ decision: "unknown" }),
    };
    let accepted: PublicationResult["publication"];
    let withdrawing: string | undefined;
    let withdrawalAcknowledged = false;
    const receive = (result: PublicationResult): PublicationResult => {
      if (
        result.publication &&
        (!sameObject(result.publication.object, object.reference) ||
          result.publication.operationId !== operationId)
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
      if (accepted && result.decision === "rejected")
        throw new FrameError(
          "invalid_plan",
          "an accepted publication cannot become rejected",
        );
      if (result.decision !== "unknown" || !accepted)
        state.outcome = immutable(result);
      if (result.decision === "accepted") accepted = state.outcome.publication;
      else if (result.decision === "rejected") owner.beginClose();
      return state.outcome;
    };
    state.observe = async () => {
      if (this.#connection === "cleanupOnly") return state.outcome;
      this.#assertConnected();
      return receive(await this.#control.publication(operationId));
    };
    state.withdraw = async () => {
      this.#assertConnected();
      if (!accepted) receive(await this.#control.publication(operationId));
      if (!accepted) return { decision: "unknown" };
      withdrawing ??= crypto.randomUUID();
      const result = await this.#control.withdraw({
        operationId: withdrawing,
        authority: this.#authority,
        publicationId: accepted.id,
      });
      if (result.decision === "accepted") {
        withdrawalAcknowledged = true;
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
      if (accepted && withdrawalAcknowledged) {
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
            publicationId: accepted.id,
            instances: [object.reference.export.instanceId],
            status: "released",
            failures: outcome.failures.map(
              (item) => `${item.phase}:${item.category}`,
            ),
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
    try {
      await owner.run(async () =>
        receive(await this.#control.publish(request)),
      );
    } catch (cause) {
      if (
        cause instanceof FrameError &&
        cause.code !== "outcome_unknown" &&
        cause.code !== "unavailable"
      )
        throw cause;
      // Preserve the original operation and object. Only observation resolves an
      // uncertain acknowledgement; a retry must never invent another identity.
    }
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
        failures: outcome.failures.map(
          (item) => `${item.phase}:${item.category}`,
        ),
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
        this.#executions.delete(entry.plan.execution.id);
      }
    });
  }

  disconnect(): void {
    if (this.#connection === "connected") this.#connection = "paused";
    this.#connectionRevision++;
  }

  async reconnect(control: HostControl): Promise<void> {
    if (this.#connection === "closed")
      throw new FrameError("closed", "runtime is closed");
    this.disconnect();
    const revision = this.#connectionRevision;
    const result = await control.reconnect({
      authority: this.#authority,
      realm: this.#realm,
      executions: [...this.#executions.values()].map(
        (entry) => entry.plan.execution,
      ),
      pendingProofs: this.#outbox.pending,
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
    for (const closeId of result.acceptedProofs)
      this.#outbox.acknowledge(closeId);
    if (sameEpoch && result.mode === "revalidated") {
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
    await this.flushProofs();
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

  flushProofs(): Promise<void> {
    return this.#outbox.flush(this.#control, this.#authority);
  }

  async close(signal?: AbortSignal): Promise<readonly CloseOutcome[]> {
    this.#connection = "closed";
    this.#connectionRevision++;
    const exits = [...this.#executions.values()].map((entry) =>
      entry.lifetime.beginClose(),
    );
    const outcomes = await Promise.all(
      exits.map((exit) => exit.settled(signal)),
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
