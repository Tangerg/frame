import { FrameError, type Cleanup, type Lifetime } from "../index.js";
import type {
  Authority,
  LocalPlan,
  PlanAdmissionDecision,
  PublicationResult,
  ReadyReport,
} from "../protocol.generated.js";
import { isControlRefusal, type HostControl } from "./control.js";

import { sameAuthority } from "./identity.js";

export class ReadySubmission {
  readonly #report: ReadyReport;
  readonly #lifetime: Lifetime;
  #dispatched = false;

  constructor(report: ReadyReport, lifetime: Lifetime) {
    this.#report = report;
    this.#lifetime = lifetime;
  }

  async submit(control: HostControl): Promise<void> {
    const previouslyDispatched = this.#dispatched;
    let result: PublicationResult;
    try {
      result = await this.#lifetime.run(() => {
        this.#dispatched = true;
        return control.ready(this.#report);
      });
    } catch (cause) {
      if (!previouslyDispatched && isControlRefusal(cause)) {
        await this.#lifetime.close();
        throw cause;
      }
      // An earlier Ready may have committed. A later refused call cannot release
      // prepared objects that exact Host Uses may already reference.
      throw new FrameError(
        "outcome_unknown",
        "ready acknowledgement is unknown; prepared execution is retained",
        { cause },
      );
    }
    if (result.decision === "rejected") {
      await this.#lifetime.close();
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
  }
}

/** An uncertain start admission cannot release its reservation or run setup. */
export class PreparationAdmission {
  readonly #plan: LocalPlan;
  readonly #release: Cleanup;
  #settle?: (decision: PlanAdmissionDecision) => void;
  #decision?: PlanAdmissionDecision;
  #pending?: Promise<PlanAdmissionDecision>;
  #waitingForDecision = false;

  constructor(
    plan: LocalPlan,
    lifetime: Lifetime,
    settle: (decision: PlanAdmissionDecision) => void,
  ) {
    this.#plan = plan;
    this.#settle = settle;
    this.#release = lifetime.cleanup(() => {
      if (this.#decision !== undefined) return;
      this.#waitingForDecision = true;
      return { status: "unconfirmed" };
    }, "execution admission");
  }

  get pending(): boolean {
    return this.#decision === undefined;
  }

  resolve(
    control: HostControl,
    authority: Authority,
    signal?: AbortSignal,
  ): Promise<PlanAdmissionDecision> {
    const result =
      this.#decision === undefined
        ? (this.#pending ??= Promise.resolve()
            .then(() => this.#resolve(control, authority))
            .finally(() => {
              this.#pending = undefined;
            }))
        : Promise.resolve(this.#decision);
    if (!signal) return result;
    return new Promise((resolve, reject) => {
      const abort = () => reject(signal.reason);
      signal.addEventListener("abort", abort, { once: true });
      if (signal.aborted) abort();
      result
        .then(resolve, reject)
        .finally(() => signal.removeEventListener("abort", abort));
    });
  }

  async #resolve(
    control: HostControl,
    authority: Authority,
  ): Promise<PlanAdmissionDecision> {
    const result = await control.admitPlan(this.#plan);
    if (
      !result ||
      !result.authority ||
      !result.realm ||
      !result.execution ||
      !sameAuthority(result.authority, authority) ||
      result.realm.id !== this.#plan.realm.id ||
      result.realm.epoch !== this.#plan.realm.epoch ||
      result.execution.id !== this.#plan.execution.id ||
      result.execution.generation !== this.#plan.execution.generation
    )
      throw new FrameError(
        "outcome_unknown",
        "host has not settled the original preparation admission",
      );
    this.#settle?.(result.decision);
    this.#decision = result.decision;
    this.#settle = undefined;
    if (this.#waitingForDecision)
      this.#release.confirmReleased(
        "Host settled the original preparation admission",
      );
    return result.decision;
  }
}
