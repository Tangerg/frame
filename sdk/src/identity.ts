import { MAX_NAME_BYTES } from "./protocol.generated.js";

export function identity(value: string, label: string): string {
  if (
    typeof value !== "string" ||
    value.length === 0 ||
    value.length > MAX_NAME_BYTES ||
    /[\p{White_Space}\p{Cc}\p{Surrogate}]/u.test(value) ||
    new TextEncoder().encode(value).byteLength > MAX_NAME_BYTES
  )
    throw new TypeError(`${label} must be a nonempty bounded UTF-8 token`);
  return value;
}
