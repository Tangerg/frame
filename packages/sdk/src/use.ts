import { FrameError } from "./errors.js";
import {
  Lifetime,
  type Awaitable,
  type Cleanup,
  type CloseOutcome,
  type Exit,
  type Release,
} from "./lifetime.js";

/** A handle borrows through the original Use; it never owns a second exit tree. */
export interface Acquired<T> {
  with<R>(work: (value: T, signal: AbortSignal) => Awaitable<R>): Promise<R>;
  close(signal?: AbortSignal): Promise<CloseOutcome>;
  wait(signal?: AbortSignal): Promise<CloseOutcome>;
}

export interface AcquiredResource<T> {
  readonly value: T;
  close(): Awaitable<void | Release>;
}

/**
 * Reserve the cleanup before acquisition starts. A late resource belongs to the
 * same Use even when cancellation wins before the caller receives its handle.
 */
export async function acquire<T>(
  owner: Lifetime,
  label: string,
  create: (
    signal: AbortSignal,
    cleanup: Cleanup,
  ) => Awaitable<AcquiredResource<T>>,
): Promise<Acquired<T>> {
  const use = owner.branch(label);
  let resource: AcquiredResource<T> | undefined;
  const cleanup = use.cleanup(async () => {
    if (!resource) return;
    const result = await resource.close();
    if (result === undefined || result.status === "released")
      resource = undefined;
    return result;
  }, label);
  try {
    await use.run(async (signal) => {
      resource = await create(signal, cleanup);
    });
    if (use.closing) {
      throw new FrameError("closed", "resource arrived after its use closed");
    }
  } catch (error) {
    const closeFailure = resourceCloseFailure(await use.close(), true);
    if (closeFailure)
      throw new AggregateError(
        [error, closeFailure],
        "resource acquisition and close failed",
      );
    throw error;
  }
  let live: Lifetime | undefined = use;
  let exit: Exit | undefined;
  const close = (): Exit => {
    if (!exit) {
      exit = live!.beginClose();
      const release = (outcome: CloseOutcome) => {
        if (outcome.status === "released") {
          live = undefined;
          resource = undefined;
        }
      };
      exit.subscribe(release);
      release(exit.outcome);
    }
    return exit;
  };
  return Object.freeze({
    with<R>(work: (value: T, signal: AbortSignal) => Awaitable<R>): Promise<R> {
      if (!live || !resource)
        throw new FrameError("closed", "acquired value is closed");
      const value = resource.value;
      return live.run((signal) => work(value, signal));
    },
    close: (signal?: AbortSignal) => close().settled(signal),
    wait: (signal?: AbortSignal) => close().wait(signal),
  });
}

export async function withResource<T, R>(
  owner: Lifetime,
  label: string,
  create: (
    signal: AbortSignal,
    cleanup: Cleanup,
  ) => Awaitable<AcquiredResource<T>>,
  work: (value: T, signal: AbortSignal) => Awaitable<R>,
): Promise<R> {
  const acquired = await acquire(owner, label, create);
  let result: R | undefined;
  let failed = false;
  let failure: unknown;
  try {
    result = await acquired.with(work);
  } catch (error) {
    failed = true;
    failure = error;
  }
  const outcome = await acquired.close();
  const releaseFailure = resourceCloseFailure(outcome, failed);
  if (failed && releaseFailure)
    throw new AggregateError(
      [failure, releaseFailure],
      "operation and resource close failed",
    );
  if (failed) throw failure;
  if (releaseFailure) throw releaseFailure;
  return result as R;
}

function resourceCloseFailure(
  outcome: CloseOutcome,
  taskFailureReported: boolean,
): FrameError | undefined {
  // A rejected borrow already reports its task error directly to the caller.
  const additionalFailure = outcome.failures.some(
    (failure) => !taskFailureReported || failure.phase !== "task",
  );
  if (
    outcome.status === "released" &&
    !additionalFailure &&
    outcome.omittedFailures === 0
  )
    return undefined;
  return new FrameError(
    outcome.status === "unconfirmed" ? "close_unconfirmed" : "close_failed",
    outcome.status === "unconfirmed"
      ? "borrowed resource release is unconfirmed"
      : "borrowed resource cleanup failed",
    { cause: outcome },
  );
}
