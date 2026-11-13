import { FrameError, type Cleanup, type Lifetime } from "../index.js";
import type {
  LocalPlan,
  PlanAdmission,
  PlanAdmissionDecision,
  DecisionResult,
  ReadyReport,
} from "../protocol.generated.js";
import {
  ControlOperation,
  isControlRefusal,
  type ControlChannel,
} from "./control.js";

import { sameAuthority } from "./identity.js";
import { observePromise } from "../promise-observation.js";
import { captureProtocol } from "../protocol-capture.js";

export class ReadySubmission {
  readonly #report: ReadyReport;
  readonly #lifetime: Lifetime;
  #dispatched = false;

  constructor(report: ReadyReport, lifetime: Lifetime) {
    this.#report = report;
    this.#lifetime = lifetime;
  }

  async submit(channel: ControlChannel): Promise<void> {
    const previouslyDispatched = this.#dispatched;
    let result: DecisionResult;
    try {
      result = await this.#lifetime.run(() => {
        this.#dispatched = true;
        return channel.observe((control, signal) =>
          control.ready(this.#report, signal),
        );
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
    try {
      result = captureProtocol("DecisionResult", result);
    } catch (cause) {
      throw new FrameError(
        "outcome_unknown",
        "ready acknowledgement is invalid; prepared execution is retained",
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
  readonly #operation = new ControlOperation<PlanAdmissionDecision>();
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
    channel: ControlChannel,
    signal?: AbortSignal,
  ): Promise<PlanAdmissionDecision> {
    const result = this.#operation.run(channel, () =>
      this.#decision === undefined
        ? this.#resolve(channel)
        : Promise.resolve(this.#decision),
    );
    return observePromise(result, signal);
  }

  async #resolve(channel: ControlChannel): Promise<PlanAdmissionDecision> {
    const received = await channel.observe((control, signal) =>
      control.admitPlan(this.#plan, signal),
    );
    let result: PlanAdmission;
    try {
      result = captureProtocol("PlanAdmission", received);
    } catch (cause) {
      throw new FrameError(
        "outcome_unknown",
        "host admission acknowledgement is invalid",
        { cause },
      );
    }
    if (
      !sameAuthority(result.authority, channel.authority) ||
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
