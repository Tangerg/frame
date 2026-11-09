export function observePromise<T>(
  work: Promise<T>,
  signal?: AbortSignal,
): Promise<T> {
  if (!signal) return work;
  const completion = Promise.withResolvers<T>();
  let observer:
    { signal: AbortSignal; completion: PromiseWithResolvers<T> } | undefined = {
    signal,
    completion,
  };
  const take = () => {
    const current = observer;
    observer = undefined;
    current?.signal.removeEventListener("abort", abort);
    return current;
  };
  const abort = () => {
    const current = take();
    current?.completion.reject(current.signal.reason);
  };
  // Only the detachable cell is captured by the pending operation. Capturing a
  // Promise executor's enclosing signal would retain an already departed waiter.
  work.then(
    (value) => take()?.completion.resolve(value),
    (error: unknown) => take()?.completion.reject(error),
  );
  if (signal.aborted) abort();
  else signal.addEventListener("abort", abort, { once: true });
  return completion.promise;
}
