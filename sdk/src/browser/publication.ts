import { FrameError } from "../errors.js";
import { freezeJSON } from "../json.js";
import {
  DecisionValues,
  type ObjectRef,
  type PublicationResult,
} from "../protocol.generated.js";
import { sameObject } from "./identity.js";

export interface PublicationHandle {
  readonly operationId: string;
  readonly object: ObjectRef;
  readonly outcome: PublicationResult;
  observe(): Promise<PublicationResult>;
  withdraw(): Promise<PublicationResult>;
}

export interface PublicationState {
  readonly receipt: PublicationReceipt;
  observe?: () => Promise<PublicationResult>;
  withdraw?: () => Promise<PublicationResult>;
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
  receive(result: PublicationResult): PublicationResult {
    this.#checkShape(result);
    this.#checkIdentity(result);
    if (result.decision !== "unknown" || this.#outcome.decision === "unknown")
      this.#outcome = freezeJSON(result);
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

export function publicationView(state: PublicationState): PublicationHandle {
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
