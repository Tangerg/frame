import { nonempty, FrameError } from "./errors.js";

export type Awaitable<T> = T | PromiseLike<T>;

export interface Failure {
  readonly phase: "task" | "cleanup" | "child";
  readonly category: string;
}

export type Release =
  | {
      readonly status: "released";
      readonly evidence?: string;
      readonly error?: unknown;
    }
  | {
      readonly status: "unconfirmed";
      readonly evidence?: string;
      readonly error?: unknown;
    };

export interface CloseOutcome {
  readonly closeId: string;
  readonly status: "released" | "unconfirmed";
  readonly pending: readonly string[];
  readonly failures: readonly Failure[];
  readonly omittedFailures: number;
}

export interface LifetimeLimits {
  readonly tasks: number;
  readonly resources: number;
  readonly children: number;
}

const DEFAULT_LIMITS: LifetimeLimits = Object.freeze({
  tasks: 1024,
  resources: 1024,
  children: 256,
});
const FAILURE_LIMIT = 32;
// An AbortSignal keeps its reason after release. A fresh Error at Close would
// also keep engine-specific call sites and their receivers alive indefinitely.
const CLOSED_REASON = Object.freeze(
  new FrameError("closed", "lifetime is closing"),
);

function failure(phase: Failure["phase"], error: unknown): Failure {
  // Long-lived reports never retain thrown values, messages, or plugin closures.
  return Object.freeze({
    phase,
    category:
      error instanceof FrameError
        ? error.code
        : error instanceof Error
          ? "exception"
          : "thrown_value",
  });
}

function abortReason(signal: AbortSignal): unknown {
  return signal.reason ?? new DOMException("waiting cancelled", "AbortError");
}

function waitSignal<T>(promise: Promise<T>, signal?: AbortSignal): Promise<T> {
  if (!signal) return promise;
  if (signal.aborted) return Promise.reject(abortReason(signal));
  return new Promise<T>((resolve, reject) => {
    const abort = () => reject(abortReason(signal));
    signal.addEventListener("abort", abort, { once: true });
    promise
      .then(resolve, reject)
      .finally(() => signal.removeEventListener("abort", abort));
  });
}

export interface Exit {
  readonly id: string;
  readonly outcome: CloseOutcome;
  settled(signal?: AbortSignal): Promise<CloseOutcome>;
  wait(signal?: AbortSignal): Promise<CloseOutcome>;
  subscribe(observer: (outcome: CloseOutcome) => void): () => void;
}

/** An exit observation has no reference back to the resource owner. */
class ExitRecord {
  readonly view: Exit;
  readonly id: string;
  #outcome: CloseOutcome;
  #first?: Promise<CloseOutcome>;
  #settleFirst?: (outcome: CloseOutcome) => void;
  #terminal?: Promise<CloseOutcome>;
  #settleTerminal?: (outcome: CloseOutcome) => void;
  #observers = new Set<(outcome: CloseOutcome) => void>();

  constructor(id: string) {
    this.id = nonempty(id, "close identity");
    this.#outcome = Object.freeze({
      closeId: id,
      status: "unconfirmed",
      pending: Object.freeze([]),
      failures: Object.freeze([]),
      omittedFailures: 0,
    });
    this.#first = new Promise((resolve) => {
      this.#settleFirst = resolve;
    });
    this.#terminal = new Promise((resolve) => {
      this.#settleTerminal = resolve;
    });
    const current = () => this.outcome;
    this.view = Object.freeze({
      id: this.id,
      get outcome() {
        return current();
      },
      settled: (signal?: AbortSignal) => this.settled(signal),
      wait: (signal?: AbortSignal) => this.wait(signal),
      subscribe: (observer: (outcome: CloseOutcome) => void) =>
        this.subscribe(observer),
    });
  }

  get outcome(): CloseOutcome {
    return this.#outcome;
  }

  settled(signal?: AbortSignal): Promise<CloseOutcome> {
    return waitSignal(this.#first ?? Promise.resolve(this.#outcome), signal);
  }

  wait(signal?: AbortSignal): Promise<CloseOutcome> {
    return waitSignal(this.#terminal ?? Promise.resolve(this.#outcome), signal);
  }

  subscribe(observer: (outcome: CloseOutcome) => void): () => void {
    if (this.#outcome.status === "released") return () => {};
    this.#observers.add(observer);
    return () => {
      this.#observers.delete(observer);
    };
  }

  /** @internal Only the original Lifetime advances its exit observation. */
  publish(outcome: CloseOutcome): void {
    if (this.#outcome.status === "released") return;
    this.#outcome = outcome;
    this.#settleFirst?.(outcome);
    this.#settleFirst = undefined;
    this.#first = undefined;
    if (outcome.status === "released") {
      this.#settleTerminal?.(outcome);
      this.#settleTerminal = undefined;
      this.#terminal = undefined;
    }
    const observers = [...this.#observers];
    if (outcome.status === "released") this.#observers.clear();
    for (const observer of observers) {
      try {
        observer(outcome);
      } catch {
        /* Observation cannot alter release evidence. */
      }
    }
  }
}

interface Resource {
  readonly label: string;
  cleanup?: () => Awaitable<void | Release>;
  attempted: boolean;
  released: boolean;
  confirm?: () => void;
}

export interface Cleanup {
  /** Only the adapter that owns the actual resource can supply this evidence. */
  confirmReleased(evidence: string): void;
}

export class Lifetime {
  readonly label: string;
  readonly #controller = new AbortController();
  readonly #limits: LifetimeLimits;
  #tasks = new Set<Promise<unknown>>();
  #resources: Resource[] = [];
  #children = new Map<Lifetime, () => void>();
  #failures: Failure[] = [];
  #omittedFailures = 0;
  #exit?: ExitRecord;
  #draining = false;
  #dirty = false;
  #parentRelease?: (outcome: CloseOutcome) => void;
  #providers = new Set<Lifetime>();
  #dependents = new Set<Lifetime>();
  #exitObservers = new Set<(outcome: CloseOutcome) => void>();

  constructor(label: string, limits: LifetimeLimits = DEFAULT_LIMITS) {
    this.label = nonempty(label, "lifetime label");
    for (const value of [limits.tasks, limits.resources, limits.children]) {
      if (!Number.isSafeInteger(value) || value < 1)
        throw new TypeError("lifetime limits must be positive safe integers");
    }
    this.#limits = Object.freeze({ ...limits });
  }

  get signal(): AbortSignal {
    return this.#controller.signal;
  }
  get closing(): boolean {
    return this.#exit !== undefined;
  }
  get exit(): Exit | undefined {
    return this.#exit?.view;
  }

  observeExit(observer: (outcome: CloseOutcome) => void): () => void {
    if (this.#exit) observer(this.#exit.outcome);
    if (this.#exit?.outcome.status === "released") return () => {};
    this.#exitObservers.add(observer);
    return () => {
      this.#exitObservers.delete(observer);
    };
  }

  assertOpen(): void {
    if (this.#exit)
      throw new FrameError("closed", "lifetime no longer accepts work");
  }

  run<T>(task: (signal: AbortSignal) => Awaitable<T>): Promise<T> {
    this.assertOpen();
    if (this.#tasks.size >= this.#limits.tasks)
      throw new FrameError("capacity", "lifetime task capacity reached");
    // Register before invoking user code: reentrant Close must see this task.
    let resolve!: (value: T | PromiseLike<T>) => void;
    let reject!: (error: unknown) => void;
    const owned = new Promise<T>((yes, no) => {
      resolve = yes;
      reject = no;
    });
    this.#tasks.add(owned);
    owned.then(
      () => {
        this.#tasks.delete(owned);
      },
      (error: unknown) => {
        this.#tasks.delete(owned);
        this.#record("task", error);
      },
    );
    try {
      resolve(task(this.signal));
    } catch (error) {
      reject(error);
    }
    return owned;
  }

  cleanup(
    cleanup: () => Awaitable<void | Release>,
    label = "resource",
  ): Cleanup {
    this.assertOpen();
    if (this.#resources.length >= this.#limits.resources)
      throw new FrameError("capacity", "lifetime resource capacity reached");
    if (typeof cleanup !== "function")
      throw new TypeError("cleanup must be a function");
    const resource: Resource = {
      label: nonempty(label, "resource label"),
      cleanup,
      attempted: false,
      released: false,
    };
    resource.confirm = () => {
      this.#resume();
    };
    this.#resources.push(resource);
    return Object.freeze({
      confirmReleased(evidence: string) {
        nonempty(evidence, "release evidence");
        if (resource.released) return;
        if (!resource.attempted)
          throw new FrameError("invalid_plan", "resource exit has not started");
        resource.released = true;
        resource.cleanup = undefined;
        const notify = resource.confirm;
        resource.confirm = undefined;
        notify?.();
      },
    });
  }

  branch(label: string): Lifetime {
    this.assertOpen();
    if (this.#children.size >= this.#limits.children)
      throw new FrameError("capacity", "lifetime child capacity reached");
    const child = new Lifetime(label, this.#limits);
    this.#children.set(child, () => {});
    child.#parentRelease = (outcome) => {
      this.#children.get(child)?.();
      this.#children.delete(child);
      for (const item of outcome.failures) {
        if (this.#failures.length < FAILURE_LIMIT) this.#failures.push(item);
        else this.#omittedFailures++;
      }
      this.#omittedFailures += outcome.omittedFailures;
      this.#resume();
    };
    return child;
  }

  /** Records an already approved resource edge; it does not resolve a provider. */
  dependsOn(provider: Lifetime): void {
    this.assertOpen();
    provider.assertOpen();
    // Parent exit joins its children just as provider exit joins its consumers.
    // Admission must consider both edges before introducing another wait.
    const reaches = (current: Lifetime, seen: Set<Lifetime>): boolean => {
      if (current === provider) return true;
      if (seen.has(current)) return false;
      seen.add(current);
      return [...current.#children.keys(), ...current.#dependents].some(
        (next) => reaches(next, seen),
      );
    };
    if (reaches(this, new Set()))
      throw new FrameError("invalid_plan", "resource dependency cycle");
    this.#providers.add(provider);
    provider.#dependents.add(this);
  }

  beginClose(): Exit {
    if (this.#exit) return this.#exit.view;
    this.#exit = new ExitRecord(crypto.randomUUID());
    this.#controller.abort(CLOSED_REASON);
    // All branches receive cancellation before any branch is awaited.
    for (const child of this.#children.keys()) {
      const exit = child.beginClose();
      const unsubscribe = exit.subscribe(() => {
        this.#resume();
      });
      this.#children.set(child, unsubscribe);
    }
    this.#resume();
    return this.#exit.view;
  }

  close(signal?: AbortSignal): Promise<CloseOutcome> {
    return this.beginClose().settled(signal);
  }
  wait(signal?: AbortSignal): Promise<CloseOutcome> {
    return this.beginClose().wait(signal);
  }

  #record(phase: Failure["phase"], error: unknown): void {
    if (this.#failures.length < FAILURE_LIMIT)
      this.#failures.push(failure(phase, error));
    else this.#omittedFailures++;
  }

  #resume(): void {
    if (!this.#exit || this.#exit.outcome.status === "released") return;
    if (this.#draining) {
      this.#dirty = true;
      return;
    }
    this.#draining = true;
    void this.#drain().finally(() => {
      this.#draining = false;
      if (this.#dirty) {
        this.#dirty = false;
        this.#resume();
      }
    });
  }

  async #drain(): Promise<void> {
    await Promise.allSettled(this.#tasks);
    const children = [...this.#children.keys()];
    await Promise.all(children.map((child) => child.close()));
    await Promise.all(
      [...this.#dependents]
        .filter((dependent) => dependent.closing)
        .map((dependent) => dependent.close()),
    );
    if (this.#children.size === 0 && this.#dependents.size === 0) {
      while (this.#resources.length) {
        const resource = this.#resources[this.#resources.length - 1]!;
        if (!resource.attempted) {
          resource.attempted = true;
          try {
            const result = await resource.cleanup!();
            if (result?.error !== undefined)
              this.#record("cleanup", result.error);
            if (result === undefined || result.status === "released")
              resource.released = true;
          } catch (error) {
            this.#record("cleanup", error);
          }
        }
        if (!resource.released) break;
        resource.cleanup = undefined;
        resource.confirm = undefined;
        this.#resources.pop();
      }
    }
    const pending = [...this.#children.keys()].map(
      (child) => `child:${child.label}`,
    );
    pending.push(
      ...[...this.#dependents].map(
        (dependent) => `dependent:${dependent.label}`,
      ),
    );
    pending.push(...this.#resources.map((resource) => resource.label));
    const outcome: CloseOutcome = Object.freeze({
      closeId: this.#exit!.id,
      status: pending.length ? "unconfirmed" : "released",
      pending: Object.freeze(pending),
      failures: Object.freeze([...this.#failures]),
      omittedFailures: this.#omittedFailures,
    });
    this.#exit!.publish(outcome);
    const observers = [...this.#exitObservers];
    if (outcome.status === "released") this.#exitObservers.clear();
    for (const observer of observers) {
      try {
        observer(outcome);
      } catch {
        /* Observation cannot alter release evidence. */
      }
    }
    if (outcome.status === "released") {
      this.#tasks.clear();
      this.#resources = [];
      this.#children.clear();
      this.#failures = [];
      for (const provider of this.#providers) {
        provider.#dependents.delete(this);
        provider.#resume();
      }
      this.#providers.clear();
      const releaseParent = this.#parentRelease;
      this.#parentRelease = undefined;
      releaseParent?.(outcome);
    }
  }
}
