import { FrameError } from "../index.js";
import { Runtime } from "./runtime.js";

export interface PumpOptions {
  readonly intervalMilliseconds: number;
  readonly preparationCapacity: number;
  onError(error: unknown): void;
}

/** Pending plans stay at the Host until their original execution settles. */
export class ControlPump {
  readonly #runtime: Runtime;
  readonly #options: PumpOptions;
  readonly #stopping = new AbortController();
  readonly #preparations = new Map<string, Promise<void>>();
  #polling?: Promise<void>;
  #proofs?: Promise<void>;
  #running?: Promise<void>;
  #closing?: Promise<void>;
  #failure?: { error: unknown };

  constructor(runtime: Runtime, options: PumpOptions) {
    if (
      !Number.isSafeInteger(options.intervalMilliseconds) ||
      options.intervalMilliseconds < 1
    )
      throw new TypeError("poll interval must be a positive safe integer");
    if (
      !Number.isSafeInteger(options.preparationCapacity) ||
      options.preparationCapacity < 1
    )
      throw new TypeError(
        "preparation capacity must be a positive safe integer",
      );
    this.#runtime = runtime;
    this.#options = Object.freeze({ ...options });
  }

  synchronize(): Promise<void> {
    if (this.#stopping.signal.aborted)
      return Promise.reject(new FrameError("closed", "control pump is closed"));
    if (this.#polling) return this.#polling;
    const completion = Promise.withResolvers<void>();
    this.#polling = completion.promise.finally(() => {
      this.#polling = undefined;
    });
    this.#flushProofs();
    this.#synchronize().then(completion.resolve, completion.reject);
    return this.#polling;
  }

  async #synchronize(): Promise<void> {
    if (this.#stopping.signal.aborted) return;
    await this.#runtime.poll((plans) => {
      if (this.#stopping.signal.aborted) return;
      for (const plan of plans) {
        if (this.#preparations.has(plan.execution.id)) continue;
        if (this.#preparations.size >= this.#options.preparationCapacity) break;
        const preparation = this.#runtime
          .prepare(plan)
          .then(
            () => {},
            (error) => this.#report(error),
          )
          .finally(() => {
            this.#preparations.delete(plan.execution.id);
            this.#flushProofs();
          });
        this.#preparations.set(plan.execution.id, preparation);
      }
    });
    this.#flushProofs();
  }

  #flushProofs(): void {
    if (
      this.#proofs ||
      this.#stopping.signal.aborted ||
      this.#runtime.connection === "paused" ||
      this.#runtime.pendingProofs.length === 0
    )
      return;
    this.#proofs = this.#runtime
      .flushProofs()
      .catch((error) => this.#report(error))
      .finally(() => {
        this.#proofs = undefined;
      });
  }

  #report(error: unknown): void {
    if (this.#stopping.signal.aborted) return;
    try {
      this.#options.onError(error);
    } catch (error) {
      this.#failure ??= { error };
      this.#stopping.abort(error);
    }
  }

  run(signal?: AbortSignal): Promise<void> {
    if (this.#running)
      return Promise.reject(
        new FrameError("invalid_plan", "control pump is already running"),
      );
    if (this.#stopping.signal.aborted)
      return Promise.reject(new FrameError("closed", "control pump is closed"));
    const completion = Promise.withResolvers<void>();
    this.#running = completion.promise;
    this.#run(signal).then(completion.resolve, completion.reject);
    return this.#running;
  }

  async #run(signal?: AbortSignal): Promise<void> {
    let closing: Promise<void> | undefined;
    const observeClose = () => {
      closing ??= this.#closing;
    };
    const stop = () => {
      // The finally block joins this same close, including its rejection.
      closing ??= this.close();
      void closing.catch(() => {});
    };
    signal?.addEventListener("abort", stop, { once: true });
    this.#stopping.signal.addEventListener("abort", observeClose, {
      once: true,
    });
    if (signal?.aborted) stop();
    try {
      while (
        !this.#stopping.signal.aborted &&
        this.#runtime.connection !== "closed"
      ) {
        try {
          await this.synchronize();
        } catch (error) {
          this.#report(error);
        }
        await delay(this.#options.intervalMilliseconds, this.#stopping.signal);
      }
    } finally {
      signal?.removeEventListener("abort", stop);
      this.#stopping.signal.removeEventListener("abort", observeClose);
      await (closing ?? this.close());
    }
  }

  close(): Promise<void> {
    if (this.#closing) return this.#closing;
    const completion = Promise.withResolvers<void>();
    this.#closing = completion.promise.catch((error) => {
      this.#closing = undefined;
      throw error;
    });
    this.#stopping.abort();
    this.#close().then(completion.resolve, completion.reject);
    return this.#closing;
  }

  async #close(): Promise<void> {
    const [resources, ...work] = await Promise.allSettled([
      this.#runtime.close(),
      this.#polling,
      this.#proofs,
      ...this.#preparations.values(),
    ]);
    const failures: unknown[] = [];
    if (this.#failure) failures.push(this.#failure.error);
    for (const result of [resources, ...work])
      if (result.status === "rejected") failures.push(result.reason);
    if (
      resources.status === "fulfilled" &&
      resources.value.some((outcome) => outcome.status !== "released")
    )
      failures.push(
        new FrameError(
          "close_unconfirmed",
          "original runtime resources have not released",
        ),
      );
    if (failures.length)
      throw new AggregateError(
        failures,
        "control pump shutdown did not complete",
      );
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
