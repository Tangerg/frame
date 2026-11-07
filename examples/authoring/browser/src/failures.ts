export interface ConnectionFailure {
  readonly name: string;
  readonly message: string;
}

const maximumFailures = 32;
const maximumFailureTextLength = 512;

function failureText(text: string): string {
  // Copy through bounded bytes so a sliced string cannot retain its original large allocation.
  const bytes = new TextEncoder().encode(
    text.slice(0, maximumFailureTextLength),
  );
  return new TextDecoder().decode(bytes);
}

export class ConnectionFailures {
  readonly #records: ConnectionFailure[] = [];
  #omitted = 0;

  report(error: unknown, observeError?: (error: unknown) => unknown): void {
    this.record(error);
    if (observeError === undefined) return;
    try {
      const observation = observeError(error);
      if (observation !== undefined)
        void Promise.resolve(observation).catch((failure: unknown) => {
          this.record(failure);
        });
    } catch (failure) {
      // Diagnostics observers have no authority to stop connection recovery.
      this.record(failure);
    }
  }

  record(error: unknown): void {
    let record: ConnectionFailure = {
      name: "ThrownValue",
      message: "the operation threw a value without readable diagnostics",
    };
    try {
      if (error instanceof Error) {
        const { name, message } = error;
        if (typeof name === "string" && typeof message === "string")
          record = {
            name: failureText(name),
            message: failureText(message),
          };
      }
    } catch {
      // A thrown proxy or accessor cannot take over the control pump's exit.
    }
    if (this.#records.length === maximumFailures) {
      this.#records.shift();
      this.#omitted = Math.min(this.#omitted + 1, Number.MAX_SAFE_INTEGER);
    }
    this.#records.push(Object.freeze(record));
  }

  get records(): readonly ConnectionFailure[] {
    return Object.freeze([...this.#records]);
  }

  get omitted(): number {
    return this.#omitted;
  }
}
