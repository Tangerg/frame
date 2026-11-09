import { captureProtocol } from "../protocol-capture.js";
import {
  FrameError,
  Lifetime,
  Point,
  sameContract,
  type Unit,
} from "../index.js";
import { canonicalJSON } from "../json.js";
import type {
  Execution,
  Import,
  LocalPlan,
  ObjectRef,
  PlanAdmissionDecision,
  PlannedUnit,
  PreparedContribution,
} from "../protocol.generated.js";
import type { HostControl } from "./control.js";
import type { ExecutionHandle } from "./runtime.js";
import { sameObject } from "./identity.js";
import { PreparationAdmission, ReadySubmission } from "./preparation.js";
import { ProofOutbox } from "./outbox.js";

interface OwnedObject {
  readonly reference: ObjectRef;
  readonly lifetime: Lifetime;
  value: unknown;
}

export type ObjectResource = Readonly<OwnedObject>;

export interface UnitResource {
  readonly unit: PlannedUnit;
  readonly lifetime: Lifetime;
}

// Terminal handles must not retain Realm indexes or authenticated control callbacks.
function executionView(
  identity: Execution,
  objects: ReadonlyMap<string, ObjectResource>,
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

function checkImport(
  binding: Import,
  planned: ReadonlyMap<string, PlannedUnit>,
  external: ReadonlyMap<string, UnitResource>,
): PlannedUnit | undefined {
  switch (binding.kind) {
    case "local":
      return localProvider(binding, planned, external);
    case "remote":
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
      return undefined;
    case "absent":
      if (binding.local || binding.remote)
        throw new FrameError("invalid_plan", "invalid import kind");
      return undefined;
    default:
      throw new FrameError("invalid_plan", "invalid import kind");
  }
}

function localProvider(
  binding: Import,
  planned: ReadonlyMap<string, PlannedUnit>,
  external: ReadonlyMap<string, UnitResource>,
): PlannedUnit {
  const local = binding.local;
  if (
    !local ||
    binding.remote ||
    !sameContract(binding.contract, local.contract)
  )
    throw new FrameError(
      "contract_mismatch",
      "local import differs from its approved contract",
    );
  const provider =
    planned.get(local.instanceId) ?? external.get(local.instanceId)?.unit;
  const exported = provider?.exports.some(
    (entry) =>
      entry.name === local.name &&
      sameContract(entry.contract, binding.contract),
  );
  if (!provider || !exported)
    throw new FrameError(
      "invalid_plan",
      "local import does not identify an exact planned export",
    );
  return provider;
}

function orderUnits(
  plan: LocalPlan,
  external: ReadonlyMap<string, UnitResource>,
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
      const provider = checkImport(binding, byId, external);
      if (provider && byId.has(provider.instance.id)) visit(provider);
    }
    visiting.delete(id);
    visited.add(id);
    ordered.push(unit);
  };
  for (const unit of plan.units) visit(unit);
  return ordered;
}

function checkDeclaredExports(unit: PlannedUnit, definition: Unit): void {
  if (Object.keys(definition.exports).length !== unit.exports.length)
    throw new FrameError(
      "contract_mismatch",
      "module exports differ from the approved declaration",
    );
  for (const declared of unit.exports) {
    const exported = definition.exports[declared.name];
    if (
      !Object.hasOwn(definition.exports, declared.name) ||
      !exported ||
      !sameContract(exported.ref, declared.contract)
    )
      throw new FrameError(
        "contract_mismatch",
        "module exports an unapproved contract",
      );
  }
}

function pointExport(exported: unknown): { metadata: unknown; value: unknown } {
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
  return exported as { metadata: unknown; value: unknown };
}

// Realm indexes only project these resources. Preparation, publication staging,
// and release all enter through the original execution's transitions.
export class ExecutionResources {
  readonly plan: LocalPlan;
  readonly lifetime: Lifetime;
  readonly admission: PreparationAdmission;
  readonly view: ExecutionHandle;
  readonly ordered: readonly PlannedUnit[];
  readonly #fingerprint: string;
  readonly #reservation: string;
  readonly #outbox: ProofOutbox;
  readonly #instanceIndex: Map<string, UnitResource>;
  readonly #objectIndex: Map<string, ObjectResource>;
  readonly #units = new Map<string, UnitResource>();
  readonly #objects = new Map<string, OwnedObject>();
  readonly #contributions: PreparedContribution[] = [];
  #preparing?: Promise<ExecutionHandle>;
  #ready?: ReadySubmission;
  #forget?: (execution: ExecutionResources) => void;

  constructor(
    plan: LocalPlan,
    instances: Map<string, UnitResource>,
    objects: Map<string, ObjectResource>,
    outbox: ProofOutbox,
    forget: (execution: ExecutionResources) => void,
  ) {
    this.plan = plan;
    this.#fingerprint = canonicalJSON(plan);
    this.ordered = orderUnits(plan, instances);
    this.#instanceIndex = instances;
    this.#objectIndex = objects;
    this.#assertUnownedInstances();
    this.#reservation = `execution:${plan.execution.id}`;
    outbox.reserve(this.#reservation);
    this.#outbox = outbox;
    this.#forget = forget;
    this.lifetime = new Lifetime(this.#reservation);
    this.view = executionView(plan.execution, this.#objects, this.lifetime);
    this.admission = new PreparationAdmission(plan, this.lifetime, (decision) =>
      this.#settleAdmission(decision),
    );
  }

  verify(plan: LocalPlan): void {
    if (canonicalJSON(plan) !== this.#fingerprint)
      throw new FrameError(
        "stale_binding",
        "execution identity cannot be relabeled or restarted",
      );
  }

  get preparing(): Promise<ExecutionHandle> | undefined {
    return this.#preparing;
  }

  get prepared(): boolean {
    return this.#ready !== undefined;
  }

  prepare(work: () => Promise<ExecutionHandle>): Promise<ExecutionHandle> {
    this.#preparing ??= Promise.resolve()
      .then(work)
      .finally(() => {
        this.#preparing = undefined;
      });
    return this.#preparing;
  }

  unit(instanceId: string): UnitResource | undefined {
    return this.#units.get(instanceId);
  }

  verifyDefinition(unit: PlannedUnit, definition: Unit): void {
    if (definition.id !== unit.instance.source.unitId)
      throw new FrameError(
        "contract_mismatch",
        "module does not declare the approved unit",
      );
    checkDeclaredExports(unit, definition);
  }

  #assertUnownedInstances(): void {
    if (this.ordered.some((unit) => this.#instanceIndex.has(unit.instance.id)))
      throw new FrameError(
        "invalid_plan",
        "a different execution already owns this instance",
      );
  }

  ownUnits(): void {
    this.lifetime.assertOpen();
    if (this.admission.pending)
      throw new FrameError(
        "invalid_plan",
        "host has not admitted this execution",
      );
    // Another admission may have completed while this execution awaited its Host.
    this.#assertUnownedInstances();
    for (const unit of this.ordered) {
      const owned = Object.freeze({
        unit,
        lifetime: this.lifetime.branch(`instance:${unit.instance.id}`),
      });
      this.#units.set(unit.instance.id, owned);
      this.#instanceIndex.set(unit.instance.id, owned);
    }
    for (const owned of this.#units.values()) {
      for (const binding of owned.unit.imports) {
        if (binding.kind !== "local") continue;
        const provider = this.#instanceIndex.get(binding.local!.instanceId);
        if (!provider)
          throw new FrameError("stale_binding", "approved provider has exited");
        owned.lifetime.dependsOn(provider.lifetime);
      }
    }
  }

  stageExports(
    unit: PlannedUnit,
    definition: Unit,
    exports: Record<string, unknown>,
  ): void {
    const owner = this.#units.get(unit.instance.id)!.lifetime;
    for (const declared of unit.exports) {
      if (!Object.hasOwn(exports, declared.name))
        throw new FrameError(
          "contract_mismatch",
          "declared export was not prepared",
        );
      const exported = exports[declared.name];
      if (declared.contract.kind === "point") {
        const { metadata, value } = pointExport(exported);
        const object = this.stageContribution(
          unit.instance.id,
          definition.exports[declared.name] as Point<unknown, unknown>,
          declared.name,
          value,
          owner,
        );
        this.#contributions.push(
          captureProtocol("PreparedContribution", {
            object: object.reference,
            metadataJson: canonicalJSON(metadata),
          }),
        );
      } else {
        if (declared.contract.kind === "service" && exported == null)
          throw new FrameError(
            "contract_mismatch",
            "service implementation is missing",
          );
        this.#retain(
          captureProtocol("ObjectRef", {
            export: {
              instanceId: unit.instance.id,
              name: declared.name,
              contract: declared.contract,
            },
            objectId: crypto.randomUUID(),
          }),
          exported,
          owner,
        );
      }
    }
  }

  stageContribution<M, V>(
    instanceId: string,
    point: Point<M, V>,
    key: string,
    value: V,
    owner: Lifetime,
  ): ObjectResource {
    owner.assertOpen();
    if (value == null)
      throw new FrameError(
        "contract_mismatch",
        "point implementation is missing",
      );
    const unit = this.#units.get(instanceId)?.unit;
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
      [...this.#objects.values()].some(
        (item) =>
          item.reference.export.instanceId === instanceId &&
          item.reference.export.name === key,
      )
    )
      throw new FrameError(
        "invalid_plan",
        "contribution key is empty or already prepared",
      );
    const reference: ObjectRef = captureProtocol("ObjectRef", {
      export: { instanceId, name: key, contract: point.ref },
      objectId: crypto.randomUUID(),
    });
    return this.#retain(reference, value, owner);
  }

  #retain(
    reference: ObjectRef,
    value: unknown,
    lifetime: Lifetime,
  ): ObjectResource {
    lifetime.assertOpen();
    const object = { reference, value, lifetime };
    this.#objects.set(reference.objectId, object);
    this.#objectIndex.set(reference.objectId, object);
    return object;
  }

  finishPreparation(): void {
    this.lifetime.assertOpen();
    if (this.#ready)
      throw new FrameError(
        "invalid_plan",
        "execution has already prepared its report",
      );
    this.#ready = new ReadySubmission(
      captureProtocol("ReadyReport", {
        operationId: crypto.randomUUID(),
        authority: this.plan.authority,
        realm: this.plan.realm,
        execution: this.plan.execution,
        objects: this.view.objects,
        contributions: this.#contributions,
      }),
      this.lifetime,
    );
    this.#contributions.length = 0;
  }

  async submitReady(control: HostControl): Promise<ExecutionHandle> {
    this.lifetime.assertOpen();
    if (!this.#ready)
      throw new FrameError("invalid_plan", "execution has no prepared report");
    await this.#ready.submit(control);
    this.lifetime.assertOpen();
    return this.view;
  }

  forgetReleasedObject(reference: ObjectRef): void {
    const object = this.#objects.get(reference.objectId);
    if (!object) return;
    if (!sameObject(object.reference, reference))
      throw new FrameError(
        "permission_denied",
        "object identity changed before release",
      );
    if (object.lifetime.exit?.outcome.status !== "released")
      throw new FrameError(
        "close_unconfirmed",
        "object owner has not confirmed release",
      );
    object.value = undefined;
    this.#objects.delete(reference.objectId);
    this.#objectIndex.delete(reference.objectId);
  }

  #settleAdmission(decision: PlanAdmissionDecision): void {
    switch (decision) {
      case "notAdmitted":
      case "settled":
        this.#outbox.cancelReservation(this.#reservation);
        this.#forget?.(this);
        this.#forget = undefined;
        this.lifetime.beginClose();
        return;
      case "admitted":
      case "cleanupRequired":
        this.#forget = undefined;
        this.#observeExit();
        if (decision === "cleanupRequired") this.lifetime.beginClose();
        return;
      default:
        throw new FrameError(
          "outcome_unknown",
          "host has not settled the original preparation admission",
        );
    }
  }

  #observeExit(): void {
    this.lifetime.observeExit((outcome) => {
      if (outcome.status !== "released") return;
      this.#outbox.record(
        this.#reservation,
        captureProtocol("CloseProof", {
          closeId: outcome.closeId,
          proofId: crypto.randomUUID(),
          authority: this.plan.authority,
          realm: this.plan.realm,
          execution: this.plan.execution,
          scopeId: this.plan.scopeId,
          scopeGeneration: this.plan.scopeGeneration,
          scopeKind: "execution",
          instances: this.ordered.map((unit) => unit.instance.id),
          status: "released",
        }),
      );
      for (const object of this.#objects.values())
        this.forgetReleasedObject(object.reference);
      for (const instanceId of this.#units.keys())
        this.#instanceIndex.delete(instanceId);
      this.#units.clear();
      this.#contributions.length = 0;
      this.#preparing = undefined;
      this.#ready = undefined;
    });
  }
}
