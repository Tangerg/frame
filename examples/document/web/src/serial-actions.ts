import type { Lifetime } from "@frame/sdk";

export class SerialActions {
  readonly #owner: Lifetime;
  #tail: Promise<void> = Promise.resolve();

  constructor(owner: Lifetime) {
    this.#owner = owner;
  }

  run<T>(work: (signal: AbortSignal) => Promise<T>): Promise<T> {
    const previous = this.#tail;
    const completion = this.#owner.run(async (signal) => {
      await previous;
      signal.throwIfAborted();
      return work(signal);
    });
    // A caller observes its own failure; the next admitted action still runs.
    this.#tail = completion.then(
      () => {},
      () => {},
    );
    return completion;
  }
}
