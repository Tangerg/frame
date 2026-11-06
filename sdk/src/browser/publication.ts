import { FrameError } from "../errors.js";
import { canonicalJSON, freezeJSON } from "../json.js";
import type { Point } from "../contracts.js";
import type { CloseOutcome, Lifetime } from "../lifetime.js";
import {
  DecisionValues,
  type Authority,
  type PublicationRequest,
  type ObjectRef,
  type PublicationResult,
} from "../protocol.generated.js";
import { sameObject } from "./identity.js";
import { isControlRefusal, type HostControl } from "./control.js";
import type { ExecutionResources } from "./execution.js";
import type { ProofOutbox } from "./outbox.js";

export interface PublicationHandle {
  readonly operationId: string;
  readonly object: ObjectRef;
  readonly outcome: PublicationResult;
  observe(): Promise<PublicationResult>;
  withdraw(): Promise<PublicationResult>;
}

export class PublicationReceipt {
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

  // Only an unknown outcome may be refined; a confirmed decision and the
  // publication it names never change.
  receive(received: PublicationResult): PublicationResult {
    const result = freezeJSON(received);
    this.#checkShape(result);
    this.#checkIdentity(result);
    if (result.decision !== "unknown" || this.#outcome.decision === "unknown")
      this.#outcome = result;
    return this.#outcome;
  }

  #checkShape(result: PublicationResult): void {
    if (!result || !DecisionValues.includes(result.decision))
      throw new FrameError("invalid_plan", "invalid publication decision");
    if (result.decision === "accepted" && !result.publication)
      throw new FrameError(
        "invalid_plan",
        "accepted publication has no exact publication identity",
      );
  }

  #checkIdentity(result: PublicationResult): void {
    const publication = result.publication;
    if (
      publication &&
      (!sameObject(publication.object, this.object) ||
        publication.operationId !== this.operationId)
    )
      throw new FrameError(
        "permission_denied",
        "publication response changed the original object or operation",
      );
    const current = this.#outcome.decision;
    if (
      current !== "unknown" &&
      result.decision !== "unknown" &&
      result.decision !== current
    )
      throw new FrameError(
        "invalid_plan",
        "a confirmed publication decision cannot change",
      );
    const accepted = this.acceptedPublication;
    if (
      accepted &&
      publication &&
      (publication.id !== accepted.id ||
        publication.revision !== accepted.revision)
    )
      throw new FrameError(
        "permission_denied",
        "publication response changed the original publication identity",
      );
  }

  uncertain(): void {
    if (this.#outcome.decision !== "unknown") return;
    this.#outcome = Object.freeze({
      decision: "unknown",
      problem: "invalid publication acknowledgement",
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

export interface PublicationChannel {
  readonly control: HostControl;
  readonly authority: Authority;
  readonly mode: "connected" | "cleanupOnly";
}

interface PublicationContent<M, V> {
  readonly instanceId: string;
  readonly point: Point<M, V>;
  readonly key: string;
  readonly metadata: M;
  readonly value: V;
}

interface ActivePublication {
  readonly request: PublicationRequest;
  readonly lifetime: Lifetime;
  readonly execution: ExecutionResources;
  readonly outbox: ProofOutbox;
  readonly reservation: string;
  readonly channel: () => PublicationChannel;
}

export class Publication<M, V> {
  readonly view: PublicationHandle;
  readonly #receipt: PublicationReceipt;
  #active?: ActivePublication;

  private constructor(
    execution: ExecutionResources,
    content: PublicationContent<M, V>,
    outbox: ProofOutbox,
    channel: () => PublicationChannel,
  ) {
    const unit = execution.unit(content.instanceId);
    if (!unit)
      throw new FrameError(
        "permission_denied",
        "publication instance is outside this execution",
      );
    const operationId = crypto.randomUUID();
    const reservation = `publication:${operationId}`;
    const metadataJson = canonicalJSON(content.metadata);
    outbox.reserve(reservation);
    let lifetime: Lifetime | undefined;
    let object: ObjectRef;
    try {
      lifetime = unit.lifetime.branch(reservation);
      object = execution.stageContribution(
        content.instanceId,
        content.point,
        content.key,
        content.value,
        lifetime,
      ).reference;
    } catch (error) {
      lifetime?.beginClose();
      outbox.cancelReservation(reservation);
      throw error;
    }
    const request: PublicationRequest = freezeJSON({
      operationId,
      authority: execution.plan.authority,
      realm: execution.plan.realm,
      execution: execution.plan.execution,
      object,
      metadataJson,
    });
    this.#receipt = new PublicationReceipt(operationId, object);
    this.#active = {
      request,
      lifetime,
      execution,
      outbox,
      reservation,
      channel,
    };
    this.view = publicationView(this.#receipt, this);
    lifetime.observeExit((outcome) => this.#release(outcome));
  }

  static start<M, V>(
    execution: ExecutionResources,
    content: PublicationContent<M, V>,
    outbox: ProofOutbox,
    channel: () => PublicationChannel,
  ): Promise<PublicationHandle> {
    return new Publication(execution, content, outbox, channel).#publish();
  }

  async #publish(): Promise<PublicationHandle> {
    const active = this.#active!;
    let result: PublicationResult;
    try {
      result = await active.lifetime.run(() =>
        active.channel().control.publish(active.request),
      );
    } catch (cause) {
      if (isControlRefusal(cause)) {
        await active.lifetime.close();
        throw cause;
      }
      // The original operation remains observable after an uncertain acknowledgement.
      return this.view;
    }
    try {
      this.#receive(result);
    } catch {
      // A malformed acknowledgement leaves the original operation observable.
      this.#receipt.uncertain();
    }
    if (this.#receipt.outcome.decision === "rejected")
      await active.lifetime.close();
    return this.view;
  }

  #receive(result: PublicationResult): PublicationResult {
    const observed = this.#receipt.receive(result);
    if (observed.decision === "rejected") this.#active?.lifetime.beginClose();
    return observed;
  }

  async observe(): Promise<PublicationResult> {
    const active = this.#active;
    if (!active) return this.#receipt.outcome;
    const channel = active.channel();
    if (channel.mode === "cleanupOnly") return this.#receipt.outcome;
    return this.#receive(
      await channel.control.publication(this.#receipt.operationId),
    );
  }

  async withdraw(): Promise<PublicationResult> {
    const active = this.#active;
    if (!active)
      throw new FrameError("closed", "publication resource has exited");
    let channel = this.#connectedChannel(active);
    if (this.#receipt.outcome.decision !== "accepted")
      this.#receive(
        await channel.control.publication(this.#receipt.operationId),
      );
    const accepted = this.#receipt.acceptedPublication;
    if (!accepted) return { decision: "unknown" };
    channel = this.#connectedChannel(active);
    const result = await channel.control.withdraw({
      authority: channel.authority,
      publicationId: accepted.id,
    });
    if (result.decision === "accepted") {
      this.#receipt.acknowledgeWithdrawal();
      await active.lifetime.close();
    }
    return result;
  }

  #connectedChannel(active: ActivePublication): PublicationChannel {
    const channel = active.channel();
    if (channel.mode !== "connected")
      throw new FrameError(
        "unavailable",
        "runtime accepts no new framework work on this control channel",
      );
    return channel;
  }

  #release(outcome: CloseOutcome): void {
    if (outcome.status !== "released" || !this.#active) return;
    const { execution, outbox, reservation } = this.#active;
    execution.forgetReleasedObject(this.#receipt.object);
    this.#active = undefined;
    const publicationId = this.#receipt.withdrawnPublicationId;
    if (publicationId) {
      outbox.record(
        reservation,
        freezeJSON({
          closeId: outcome.closeId,
          proofId: crypto.randomUUID(),
          authority: execution.plan.authority,
          realm: execution.plan.realm,
          execution: execution.plan.execution,
          scopeId: execution.plan.scopeId,
          scopeGeneration: execution.plan.scopeGeneration,
          scopeKind: "publication",
          publicationId,
          instances: [this.#receipt.object.export.instanceId],
          status: "released",
        }),
      );
    } else {
      // Only an acknowledged withdrawal has an independent publication exit.
      // Execution retirement covers accepted or uncertain objects not withdrawn.
      outbox.cancelReservation(reservation);
    }
  }
}

function publicationView(
  receipt: PublicationReceipt,
  owner: Pick<Publication<unknown, unknown>, "observe" | "withdraw">,
): PublicationHandle {
  return Object.freeze({
    operationId: receipt.operationId,
    object: receipt.object,
    get outcome() {
      return receipt.outcome;
    },
    observe: () => owner.observe(),
    withdraw: () => owner.withdraw(),
  });
}
