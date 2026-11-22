import { type Awaitable } from "../index.js";
import { ProcessError } from "./peer.js";
import wire from "./wire.json" with { type: "json" };

// Each call owns its accepted deliveries and first failure. A producer's final
// result cannot revoke a rejection or bypass a consumer still using the chunk.
export class StreamCall {
  readonly #stop = new AbortController();
  readonly signal: AbortSignal;
  #closed = false;
  #pending?: Promise<void>;
  #failure?: ProcessError;

  constructor(parent: AbortSignal) {
    this.signal = AbortSignal.any([parent, this.#stop.signal]);
  }

  #fail(cause: unknown): ProcessError {
    this.#failure ??= ProcessError.afterDispatch(cause);
    this.#stop.abort(this.#failure);
    return this.#failure;
  }

  deliver(
    wait: AbortSignal,
    consume: (signal: AbortSignal) => Awaitable<void>,
  ): Promise<void> {
    let delivery: Promise<void>;
    if (this.#closed) {
      delivery = Promise.reject(
        new ProcessError(wire.errors.closed, "stream is closed"),
      );
    } else if (this.#pending) {
      delivery = Promise.reject(
        this.#fail(new Error("concurrent chunk delivery")),
      );
    } else {
      const active = AbortSignal.any([this.signal, wait]);
      delivery = Promise.resolve()
        .then(async () => {
          try {
            active.throwIfAborted();
            await consume(active);
            active.throwIfAborted();
          } catch (cause) {
            throw this.#fail(cause);
          }
        })
        .finally(() => {
          this.#pending = undefined;
        });
      this.#pending = delivery;
    }
    // The call still observes a rejected delivery when its producer forgets to
    // await emit; an unhandled promise must not become a second failure path.
    void delivery.catch(() => {});
    return delivery;
  }

  async run<T>(invoke: (signal: AbortSignal) => Promise<T>): Promise<T> {
    let response:
      { status: "returned"; value: T } | { status: "failed"; cause: unknown };
    try {
      response = { status: "returned", value: await invoke(this.signal) };
    } catch (cause) {
      response = { status: "failed", cause };
      this.#stop.abort(cause);
    }
    this.#closed = true;
    const pending = this.#pending;
    if (pending) {
      this.#fail(
        new Error(
          "stream ended with pending delivery",
          response.status === "failed" ? { cause: response.cause } : undefined,
        ),
      );
      await pending.catch(() => {});
    }
    this.#stop.abort(new ProcessError(wire.errors.closed, "stream is closed"));
    if (this.#failure) throw this.#failure;
    if (response.status === "failed") throw response.cause;
    return response.value;
  }
}
