import type {
  Authority,
  ControlInputs,
  ControlOutputs,
} from "../protocol.generated.js";
import { FrameError } from "../errors.js";
import { captureProtocol } from "../protocol-capture.js";
import { observePromise } from "../promise-observation.js";

/** The application supplies an authenticated transport to its single Go Host. */
export type HostControl = {
  [K in keyof ControlInputs]: ControlInputs[K] extends Record<string, never>
    ? (signal?: AbortSignal) => Promise<ControlOutputs[K]>
    : (
        input: ControlInputs[K],
        signal?: AbortSignal,
      ) => Promise<ControlOutputs[K]>;
};

// Request signals can outlive their channel; their reason must not retain its stack receivers.
const RETIRED_OBSERVATION = Object.freeze(
  new FrameError("outcome_unknown", "control channel observation was retired"),
);

/** Retiring a channel ends its observations; its transport still owns dispatched requests. */
export class ControlChannel {
  readonly authority: Authority;
  readonly control: HostControl;
  readonly #retired = new AbortController();

  constructor(authority: Authority, control: HostControl) {
    this.authority = captureProtocol("Authority", authority);
    this.control = control;
  }

  observe<T>(
    work: (control: HostControl, signal: AbortSignal) => Promise<T>,
  ): Promise<T> {
    if (this.#retired.signal.aborted)
      return Promise.reject(this.#retired.signal.reason);
    try {
      return observePromise(
        work(this.control, this.#retired.signal),
        this.#retired.signal,
      );
    } catch (error) {
      return Promise.reject(error);
    }
  }

  retire(): void {
    if (this.#retired.signal.aborted) return;
    this.#retired.abort(RETIRED_OBSERVATION);
  }
}

/** A replacement may resume an original operation after its prior observation exits. */
export class ControlOperation<T> {
  #pending?: { readonly channel: ControlChannel; readonly work: Promise<T> };

  run(channel: ControlChannel, work: () => Promise<T>): Promise<T> {
    const previous = this.#pending;
    if (previous) {
      if (previous.channel === channel) return previous.work;
      const resume = () => this.run(channel, work);
      return previous.work.then(resume, resume);
    }
    const completion = Promise.withResolvers<T>();
    const pending = {
      channel,
      work: completion.promise.finally(() => {
        if (this.#pending === pending) this.#pending = undefined;
      }),
    };
    this.#pending = pending;
    try {
      work().then(completion.resolve, completion.reject);
    } catch (error) {
      completion.reject(error);
    }
    return pending.work;
  }
}

// A refusal excludes effects from this call, not from earlier uncertain dispatches.
export function isControlRefusal(error: unknown): boolean {
  try {
    return (
      error instanceof FrameError &&
      [
        "permission_denied",
        "invalid_plan",
        "contract_mismatch",
        "stale_binding",
        "capacity",
        "closed",
      ].includes(error.code)
    );
  } catch {
    // Failed inspection cannot prove that a mutation was refused before dispatch.
    return false;
  }
}
