import { FrameError } from "@frame/sdk";
import { sameAuthority, type HostControl } from "./control.js";
import { Runtime } from "./runtime.js";

export interface PumpOptions {
  readonly intervalMilliseconds: number;
  onError(error: unknown): void;
}

/** The pending projection is complete; no missed event authorizes new work. */
export class ControlPump {
  readonly #runtime: Runtime;
  readonly #control: HostControl;
  readonly #options: PumpOptions;
  #polling?: Promise<void>;

  constructor(runtime: Runtime, control: HostControl, options: PumpOptions) {
    if (
      !Number.isSafeInteger(options.intervalMilliseconds) ||
      options.intervalMilliseconds < 1
    )
      throw new TypeError("poll interval must be a positive safe integer");
    this.#runtime = runtime;
    this.#control = control;
    this.#options = options;
  }

  synchronize(): Promise<void> {
    this.#polling ??= this.#synchronize().finally(() => {
      this.#polling = undefined;
    });
    return this.#polling;
  }

  async #synchronize(): Promise<void> {
    if (this.#runtime.connection === "paused")
      await this.#runtime.reconnect(this.#control);
    const poll = await this.#control.poll();
    if (
      poll.realm.id !== this.#runtime.realm.id ||
      poll.realm.epoch !== this.#runtime.realm.epoch
    )
      throw new FrameError(
        "permission_denied",
        "pending plans belong to another realm",
      );
    if (!sameAuthority(poll.authority, this.#runtime.authority))
      throw new FrameError(
        "unavailable",
        "pending plans require authority revalidation",
      );
    for (const fence of poll.fences)
      this.#runtime.fence(fence.scopeId, fence.scopeGeneration);
    if (this.#runtime.connection === "connected") {
      for (const plan of poll.plans) {
        if (
          poll.fences.some(
            (fence) =>
              fence.scopeId === plan.scopeId &&
              fence.scopeGeneration === plan.scopeGeneration,
          )
        )
          continue;
        await this.#runtime.prepare(plan);
      }
    }
    await this.#runtime.flushProofs();
  }

  async run(signal: AbortSignal): Promise<void> {
    while (!signal.aborted && this.#runtime.connection !== "closed") {
      try {
        await this.synchronize();
      } catch (error) {
        this.#runtime.disconnect();
        if (!signal.aborted) this.#options.onError(error);
      }
      if (!signal.aborted)
        await delay(this.#options.intervalMilliseconds, signal);
    }
  }
}

function delay(milliseconds: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) {
      resolve();
      return;
    }
    const complete = () => {
      clearTimeout(timer);
      signal.removeEventListener("abort", complete);
      resolve();
    };
    const timer = setTimeout(complete, milliseconds);
    signal.addEventListener("abort", complete, { once: true });
  });
}
