import { DIGEST_PATTERN, MAX_NAME_BYTES } from "./protocol.generated.js";

const digestPattern = new RegExp(DIGEST_PATTERN);

/** Frame's single name rule, projected from Go's `frame.ValidName`. */
export function validName(value: unknown): value is string {
  return (
    typeof value === "string" &&
    value.length > 0 &&
    value.length <= MAX_NAME_BYTES &&
    !/[\p{White_Space}\p{Cc}\p{Surrogate}]/u.test(value) &&
    new TextEncoder().encode(value).byteLength <= MAX_NAME_BYTES
  );
}

/** A content digest in Go's canonical `value.Digest` spelling. */
export function validDigest(value: unknown): value is string {
  return typeof value === "string" && digestPattern.test(value);
}

export function identity(value: string, label: string): string {
  if (!validName(value))
    throw new TypeError(`${label} must be a nonempty bounded UTF-8 token`);
  return value;
}
