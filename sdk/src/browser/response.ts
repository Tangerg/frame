export async function readResponseBytes(
  response: Response,
  maximumBytes: number,
): Promise<Uint8Array<ArrayBuffer>> {
  if (!Number.isSafeInteger(maximumBytes) || maximumBytes < 0)
    throw new TypeError("response limit must be a nonnegative safe integer");
  if (!response.body) throw new Error("response body is missing");
  const reader = response.body.getReader();
  let bytes = new Uint8Array(0);
  let length = 0;
  try {
    for (;;) {
      const next = await reader.read();
      if (next.done) return bytes.subarray(0, length);
      const required = length + next.value.byteLength;
      if (required > maximumBytes)
        throw new Error("response exceeds its configured byte limit");
      if (required > bytes.length) {
        // Bound allocation by consumed bytes even for a stream of tiny chunks.
        const expanded = new Uint8Array(
          Math.min(maximumBytes, Math.max(required, 2 * bytes.length)),
        );
        expanded.set(bytes);
        bytes = expanded;
      }
      bytes.set(next.value, length);
      length = required;
    }
  } catch (error) {
    await reader.cancel(error);
    throw error;
  } finally {
    reader.releaseLock();
  }
}
