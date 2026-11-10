import { FrameError } from "../index.js";
import { canonicalJSON } from "../json.js";
import { captureProtocol } from "../protocol-capture.js";
import type {
  CloseProof,
  ProofAcknowledgement,
} from "../protocol.generated.js";
import type { ControlChannel } from "./control.js";
import { sameAuthority } from "./identity.js";

const SUPERSEDED_DELIVERY = Object.freeze(
  new FrameError("outcome_unknown", "proof delivery attempt was superseded"),
);

class ProofDelivery {
  readonly completion = Promise.withResolvers<void>();
  readonly replacement = Promise.withResolvers<{ work: Promise<void> }>();
  readonly stopping = new AbortController();

  constructor(readonly channel: ControlChannel) {}

  supersede(next: ProofDelivery): void {
    this.replacement.resolve({ work: next.completion.promise });
    this.stopping.abort(SUPERSEDED_DELIVERY);
  }
}

interface PendingProof {
  readonly proof: CloseProof;
  readonly acknowledged: PromiseWithResolvers<void>;
}

type ProofReservation =
  | { readonly status: "reserved" }
  | { readonly status: "recorded"; readonly entry: PendingProof };

/** Realm bootstrap owns delivery; proof delivery is outside plugin Lifetimes. */
export class ProofOutbox {
  readonly #capacity: number;
  readonly #reservations = new Map<string, ProofReservation>();
  #delivery?: ProofDelivery;

  constructor(capacity: number) {
    if (!Number.isSafeInteger(capacity) || capacity < 1)
      throw new TypeError("proof capacity must be a positive safe integer");
    this.#capacity = capacity;
  }

  reserved(identity: string): boolean {
    return this.#reservations.has(identity);
  }

  reserve(identity: string): void {
    if (this.#reservations.has(identity))
      throw new FrameError(
        "invalid_plan",
        "proof scope identity is already owned",
      );
    if (this.#reservations.size >= this.#capacity)
      throw new FrameError("capacity", "unacknowledged proof capacity reached");
    this.#reservations.set(identity, Object.freeze({ status: "reserved" }));
  }

  /** Only for an uncreated resource or one covered by its owning execution proof. */
  cancelReservation(identity: string): void {
    if (this.#reservations.get(identity)?.status === "recorded")
      throw new FrameError(
        "invalid_plan",
        "recorded evidence must receive its own acknowledgement",
      );
    this.#reservations.delete(identity);
  }

  record(reservation: string, source: CloseProof): void {
    const proof = captureProtocol("CloseProof", source);
    const current = this.#reservations.get(reservation);
    if (!current)
      throw new FrameError(
        "invalid_plan",
        "proof scope was not reserved before creation",
      );
    if (proof.status !== "released")
      throw new FrameError(
        "close_unconfirmed",
        "only confirmed release is exit evidence",
      );
    if (current.status === "recorded") {
      if (canonicalJSON(current.entry.proof) !== canonicalJSON(proof))
        throw new FrameError(
          "invalid_plan",
          "exit evidence identity cannot change",
        );
      return;
    }
    for (const other of this.#reservations.values()) {
      if (
        other.status === "recorded" &&
        other.entry.proof.proofId === proof.proofId
      )
        throw new FrameError(
          "invalid_plan",
          "exit evidence identity already belongs to another scope",
        );
    }
    const recorded: ProofReservation = Object.freeze({
      status: "recorded",
      entry: Object.freeze({
        proof,
        acknowledged: Promise.withResolvers<void>(),
      }),
    });
    // Delivery follows evidence creation, not the earlier capacity reservation.
    this.#reservations.delete(reservation);
    this.#reservations.set(reservation, recorded);
  }

  get pending(): readonly CloseProof[] {
    return Object.freeze(
      [...this.#reservations.values()].flatMap((state) =>
        state.status === "recorded" ? [state.entry.proof] : [],
      ),
    );
  }
  get empty(): boolean {
    return this.#reservations.size === 0;
  }

  flush(
    channel: ControlChannel,
    acknowledged: (proof: CloseProof) => void,
  ): Promise<void> {
    if (this.#delivery?.channel === channel)
      return this.#delivery.completion.promise;
    const previous = this.#delivery;
    const delivery = new ProofDelivery(channel);
    this.#delivery = delivery;
    previous?.supersede(delivery);
    void this.#deliver(delivery, acknowledged);
    return delivery.completion.promise;
  }

  async #deliver(
    delivery: ProofDelivery,
    acknowledged: (proof: CloseProof) => void,
  ): Promise<void> {
    try {
      // A previously unrecorded reservation may become ready during the send.
      // Rescan the owner after each acknowledgement instead of skipping it forever.
      for (let next = this.#nextPending(); next; next = this.#nextPending()) {
        const [reservation, entry] = next;
        const sent = this.#send(delivery, reservation, entry, acknowledged);
        const result = await Promise.race([
          sent,
          entry.acknowledged.promise,
          delivery.replacement.promise,
        ]);
        if (result || this.#delivery !== delivery) {
          const replacement = result ?? (await delivery.replacement.promise);
          await replacement.work;
          break;
        }
      }
      // Complete a current delivery in the same turn as its last ready check;
      // a new proof must never join a drain that already decided it had no work.
      if (this.#delivery === delivery) this.#delivery = undefined;
      delivery.completion.resolve();
    } catch (error) {
      if (this.#delivery === delivery) this.#delivery = undefined;
      delivery.completion.reject(error);
    }
  }

  #nextPending():
    readonly [reservation: string, entry: PendingProof] | undefined {
    for (const [reservation, state] of this.#reservations) {
      if (state.status === "recorded") return [reservation, state.entry];
    }
    return undefined;
  }

  #ownsProof(reservation: string, entry: PendingProof): boolean {
    const current = this.#reservations.get(reservation);
    return current?.status === "recorded" && current.entry === entry;
  }

  async #send(
    delivery: ProofDelivery,
    reservation: string,
    entry: PendingProof,
    acknowledged: (proof: CloseProof) => void,
  ): Promise<void> {
    const { channel } = delivery;
    let ack: ProofAcknowledgement;
    try {
      ack = captureProtocol(
        "ProofAcknowledgement",
        await channel.control.prove(entry.proof, delivery.stopping.signal),
      );
    } catch (error) {
      if (!this.#ownsProof(reservation, entry)) return;
      throw error;
    }
    if (!this.#ownsProof(reservation, entry)) return;
    if (
      ack.closeId !== entry.proof.closeId ||
      ack.proofId !== entry.proof.proofId ||
      !sameAuthority(ack.authority, channel.authority) ||
      !ack.recorded
    )
      throw new FrameError(
        "outcome_unknown",
        "host has not durably acknowledged this exit proof",
      );
    // A retired transport can still confirm this exact evidence. It cannot
    // remove another attempt or repeat the acknowledgement transition.
    this.#reservations.delete(reservation);
    entry.acknowledged.resolve();
    acknowledged(entry.proof);
  }
}
