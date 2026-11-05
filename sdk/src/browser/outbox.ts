import { FrameError } from "../index.js";
import type { Authority, CloseProof } from "../protocol.generated.js";
import type { HostControl } from "./control.js";
import { sameAuthority } from "./identity.js";

/** Realm bootstrap owns delivery; proof delivery is outside plugin Lifetimes. */
export class ProofOutbox {
  readonly #capacity: number;
  #reserved = new Set<string>();
  #pending = new Map<string, { reservation: string; proof: CloseProof }>();
  #sending?: Promise<void>;

  constructor(capacity: number) {
    if (!Number.isSafeInteger(capacity) || capacity < 1)
      throw new TypeError("proof capacity must be a positive safe integer");
    this.#capacity = capacity;
  }

  reserved(identity: string): boolean {
    return this.#reserved.has(identity);
  }

  reserve(identity: string): void {
    if (this.#reserved.has(identity))
      throw new FrameError(
        "invalid_plan",
        "proof scope identity is already owned",
      );
    if (this.#reserved.size >= this.#capacity)
      throw new FrameError("capacity", "unacknowledged proof capacity reached");
    this.#reserved.add(identity);
  }

  /** Only for an uncreated resource or one covered by its owning execution proof. */
  cancelReservation(identity: string): void {
    if (
      [...this.#pending.values()].some(
        (entry) => entry.reservation === identity,
      )
    )
      throw new FrameError(
        "invalid_plan",
        "recorded evidence must receive its own acknowledgement",
      );
    this.#reserved.delete(identity);
  }

  record(reservation: string, proof: CloseProof): void {
    if (!this.#reserved.has(reservation))
      throw new FrameError(
        "invalid_plan",
        "proof scope was not reserved before creation",
      );
    if (proof.status !== "released")
      throw new FrameError(
        "close_unconfirmed",
        "only confirmed release is exit evidence",
      );
    const previous = this.#pending.get(proof.proofId);
    if (previous) {
      if (JSON.stringify(previous.proof) !== JSON.stringify(proof))
        throw new FrameError(
          "invalid_plan",
          "exit evidence identity cannot change",
        );
      return;
    }
    this.#pending.set(proof.proofId, {
      reservation,
      proof: freezeProof(proof),
    });
  }

  get pending(): readonly CloseProof[] {
    return Object.freeze(
      [...this.#pending.values()].map((entry) => entry.proof),
    );
  }
  get empty(): boolean {
    return this.#reserved.size === 0;
  }

  flush(
    control: HostControl,
    authority: Authority,
    acknowledged: (proof: CloseProof) => void,
  ): Promise<void> {
    if (this.#sending) return this.#sending;
    const completion = Promise.withResolvers<void>();
    this.#sending = completion.promise.finally(() => {
      this.#sending = undefined;
    });
    this.#flush(control, authority, acknowledged).then(
      completion.resolve,
      completion.reject,
    );
    return this.#sending;
  }

  async #flush(
    control: HostControl,
    authority: Authority,
    acknowledged: (proof: CloseProof) => void,
  ): Promise<void> {
    for (const entry of this.#pending.values()) {
      const ack = await control.prove(entry.proof);
      if (
        ack.closeId !== entry.proof.closeId ||
        ack.proofId !== entry.proof.proofId ||
        !sameAuthority(ack.authority, authority) ||
        !ack.recorded
      ) {
        throw new FrameError(
          "outcome_unknown",
          "host has not durably acknowledged this exit proof",
        );
      }
      this.#pending.delete(ack.proofId);
      this.#reserved.delete(entry.reservation);
      acknowledged(entry.proof);
    }
  }
}

function freezeProof(proof: CloseProof): CloseProof {
  return Object.freeze({
    ...proof,
    authority: Object.freeze({ ...proof.authority }),
    realm: Object.freeze({ ...proof.realm }),
    execution: Object.freeze({ ...proof.execution }),
    instances: Object.freeze([...proof.instances]),
  });
}
