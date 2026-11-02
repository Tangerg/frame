import { FrameError, type Cleanup, type Lifetime } from "../index.js";
import type {
  Authority,
  LocalPlan,
  PlanAdmissionDecision,
} from "../protocol.generated.js";
import { sameAuthority, type HostControl } from "./control.js";

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
