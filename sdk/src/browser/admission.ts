import { FrameError, type Lifetime } from "../index.js";
import { canonicalJSON } from "../json.js";
import type {
  AcquireRequest,
  Execution,
  UseGrant,
} from "../protocol.generated.js";
import type { Use } from "./runtime.js";
import { isControlRefusal, type HostControl } from "./control.js";

// Both acquisition entrances observe one original operation. A terminal owner
// remains here until its proof is acknowledged; historical receipts alone never
// authorize another materialization after that bounded record is discarded.
export class UseAdmission {
  readonly operationId: string;
  readonly parentUseId: string | undefined;
  readonly execution: Execution;
  readonly lifetime: Lifetime;
  readonly reservation: string;
  #request?: AcquireRequest;
  #requestDispatched = false;
  #grant?: UseGrant;
  #use?: Use;
  #pending?: Promise<Use>;

  constructor(
    identity: Pick<AcquireRequest, "operationId" | "parentUseId" | "execution">,
    lifetime: Lifetime,
    reservation: string,
  ) {
    this.operationId = identity.operationId;
    this.parentUseId = identity.parentUseId;
    this.execution = identity.execution;
    this.lifetime = lifetime;
    this.reservation = reservation;
  }

  get use(): Use | undefined {
    return this.#use;
  }

  get grant(): UseGrant | undefined {
    return this.#grant;
  }

  get hasRequest(): boolean {
    return this.#request !== undefined;
  }

  verifyIdentity(
    identity: Pick<AcquireRequest, "operationId" | "parentUseId" | "execution">,
  ): void {
    if (
      identity.operationId !== this.operationId ||
      identity.parentUseId !== this.parentUseId ||
      identity.execution.id !== this.execution.id ||
      identity.execution.generation !== this.execution.generation
    )
      throw new FrameError(
        "permission_denied",
        "use operation cannot change its original owner",
      );
  }

  bindRequest(request: AcquireRequest): void {
    this.verifyIdentity(request);
    if (
      this.#request &&
      canonicalJSON(this.#request) !== canonicalJSON(request)
    )
      throw new FrameError(
        "invalid_plan",
        "admission operation cannot be relabeled",
      );
    this.#request = request;
  }

  async requestGrant(control: HostControl): Promise<UseGrant> {
    if (!this.#request)
      throw new FrameError("invalid_plan", "acquisition request is required");
    const previouslyDispatched = this.#requestDispatched;
    this.#requestDispatched = true;
    try {
      return await control.acquire(this.#request);
    } catch (error) {
      // A later refusal cannot disprove an earlier acceptance whose ACK was lost.
      if (!previouslyDispatched && isControlRefusal(error))
        this.lifetime.beginClose();
      throw error;
    }
  }

  verifyGrant(grant: UseGrant): void {
    this.verifyIdentity(grant);
    if (this.#grant && canonicalJSON(this.#grant) !== canonicalJSON(grant))
      throw new FrameError(
        "permission_denied",
        "use identity cannot be relabeled",
      );
  }

  materialize(grant: UseGrant, use: Use): void {
    this.verifyGrant(grant);
    if (this.#use)
      throw new FrameError("invalid_plan", "use is already materialized");
    this.#grant = grant;
    this.#use = use;
  }

  resolve(work: () => Promise<Use>): Promise<Use> {
    if (this.#use) return Promise.resolve(this.#use);
    if (this.#pending) return this.#pending;
    let resolve!: (use: Use) => void;
    let reject!: (cause: unknown) => void;
    const result = new Promise<Use>((accept, refuse) => {
      resolve = accept;
      reject = refuse;
    });
    this.#pending = result.finally(() => {
      this.#pending = undefined;
    });
    try {
      this.lifetime.run(work).then(resolve, reject);
    } catch (cause) {
      reject(cause);
    }
    return this.#pending;
  }
}
