import { FrameError, isErrorCode, type ErrorCode } from "./errors.js";
import type { ReleaseState } from "./protocol.generated.js";

export type Awaitable<T> = T | PromiseLike<T>;

export interface Failure {
  readonly phase: "task" | "cleanup" | "child";
  readonly category: ErrorCode | "exception" | "thrown_value";
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
  readonly status: ReleaseState;
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
const maximumLifetimeTextCodeUnits = 1024;
// An AbortSignal keeps its reason after release. A fresh Error at Close would
// also keep engine-specific call sites and their receivers alive indefinitely.
const CLOSED_REASON = Object.freeze(
  new FrameError("closed", "lifetime is closing"),
);

function boundedLifetimeText(value: string, label: string): string {
  if (
    typeof value !== "string" ||
    !value.trim() ||
    value.length > maximumLifetimeTextCodeUnits
  )
    throw new TypeError(`${label} must be a nonempty bounded string`);
  return value;
}

function failure(phase: Failure["phase"], error: unknown): Failure {
  // Long-lived reports never retain thrown values, messages, or plugin closures.
  return Object.freeze({ phase, category: failureCategory(error) });
}

function failureCategory(error: unknown): Failure["category"] {
  try {
    if (error instanceof FrameError) {
      const code = error.code;
      return isErrorCode(code) ? code : "exception";
    }
    return error instanceof Error ? "exception" : "thrown_value";
  } catch {
    // Thrown proxies and accessors cannot interrupt the original resource's exit.
    return "thrown_value";
  }
}

function abortReason(signal: AbortSignal): unknown {
  return signal.reason ?? new DOMException("waiting cancelled", "AbortError");
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
  #published = false;
  #observers = new Set<(outcome: CloseOutcome) => void>();

  constructor(id: string) {
    this.id = boundedLifetimeText(id, "close identity");
    this.#outcome = Object.freeze({
      closeId: id,
      status: "unconfirmed",
      pending: Object.freeze([]),
      failures: Object.freeze([]),
      omittedFailures: 0,
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
    return this.#waitFor(false, signal);
  }

  wait(signal?: AbortSignal): Promise<CloseOutcome> {
    return this.#waitFor(true, signal);
  }

  #waitFor(terminal: boolean, signal?: AbortSignal): Promise<CloseOutcome> {
    if (signal?.aborted) return Promise.reject(abortReason(signal));
    if (this.#published && (!terminal || this.#outcome.status === "released"))
      return Promise.resolve(this.#outcome);
    return new Promise((resolve, reject) => {
      const abort = () => {
        unsubscribe();
        reject(abortReason(signal!));
      };
      const unsubscribe = this.subscribe((outcome) => {
        if (terminal && outcome.status !== "released") return;
        unsubscribe();
        signal?.removeEventListener("abort", abort);
        resolve(outcome);
      });
      signal?.addEventListener("abort", abort, { once: true });
    });
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
    this.#published = true;
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

/** A resource's cleanup progress; release proof may also arrive from its adapter. */
type CleanupState = "pending" | "awaitingProof" | "released";

class OwnedResource {
  readonly label: string;
  #cleanup?: () => Awaitable<void | Release>;
  #state: CleanupState = "pending";
  #onConfirm?: () => void;

  constructor(
    label: string,
    cleanup: () => Awaitable<void | Release>,
    onConfirm: () => void,
  ) {
    this.label = label;
    this.#cleanup = cleanup;
    this.#onConfirm = onConfirm;
  }

  get released(): boolean {
    return this.#state === "released";
  }

  /** Runs cleanup at most once; a failure is recorded, never thrown. */
  async attempt(record: (error: unknown) => void): Promise<void> {
    if (this.#state !== "pending") return;
    this.#state = "awaitingProof";
    try {
      const result = await this.#cleanup!();
      if (result?.error !== undefined) record(result.error);
      if (result === undefined || result.status === "released") this.#release();
    } catch (error) {
      record(error);
    }
  }

  confirm(evidence: string): void {
    boundedLifetimeText(evidence, "release evidence");
    if (this.#state === "released") return;
    if (this.#state === "pending")
      throw new FrameError("invalid_plan", "resource exit has not started");
    this.#release();
    const notify = this.#onConfirm;
    this.#onConfirm = undefined;
    notify?.();
  }

  forget(): void {
    this.#onConfirm = undefined;
  }

  #release(): void {
    this.#state = "released";
    this.#cleanup = undefined;
  }
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
  #resources: OwnedResource[] = [];
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
    this.label = boundedLifetimeText(label, "lifetime label");
    const captured = {
      tasks: limits.tasks,
      resources: limits.resources,
      children: limits.children,
    };
    for (const value of Object.values(captured)) {
      if (!Number.isSafeInteger(value) || value < 1)
        throw new TypeError("lifetime limits must be positive safe integers");
    }
    this.#limits = Object.freeze(captured);
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
    const resource = new OwnedResource(
      boundedLifetimeText(label, "resource label"),
      cleanup,
      () => this.#resume(),
    );
    this.#resources.push(resource);
    return Object.freeze({
      confirmReleased: (evidence: string) => resource.confirm(evidence),
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

  // Earlier resources may be providers; retain them until dependent release is proven.
  async #releaseResources(): Promise<void> {
    while (this.#resources.length) {
      const resource = this.#resources[this.#resources.length - 1]!;
      await resource.attempt((error) => this.#record("cleanup", error));
      if (!resource.released) return;
      resource.forget();
      this.#resources.pop();
    }
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
    if (this.#children.size === 0 && this.#dependents.size === 0)
      await this.#releaseResources();
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
