export function observeCallback<T>(
  callback: (value: T) => unknown,
  value: T,
  onFailure: (error: unknown) => void,
): void {
  try {
    const result = callback(value);
    if (result !== undefined) void Promise.resolve(result).catch(onFailure);
  } catch (error) {
    onFailure(error);
  }
}
