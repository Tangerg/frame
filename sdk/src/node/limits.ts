import {
  ExactNumber,
  isJSONNumber,
  parseJSON,
  stringifyJSON,
} from "../json.js";
import profile from "./profile.generated.json" with { type: "json" };

// This is a generated Go profile, not a second configurable SDK policy.
export const PORTABLE_LIMITS = Object.freeze({ ...profile.limits });

export function exceeded(name: string, maximum: number): never {
  throw new RangeError(
    `portable: profile limit exceeded: ${name} exceeds ${maximum}`,
  );
}

function checkNumber(number: string): void {
  if (number.length > PORTABLE_LIMITS.maxNumberBytes)
    exceeded("number literal bytes", PORTABLE_LIMITS.maxNumberBytes);
  const index = number.search(/[eE]/);
  if (index < 0) return;
  const exponent = number.slice(index + 1).replace(/^[+-]?0*/, "");
  const maximum = String(PORTABLE_LIMITS.maxAbsExponent);
  if (
    exponent.length > maximum.length ||
    (exponent.length === maximum.length && exponent > maximum)
  )
    exceeded("absolute number exponent", PORTABLE_LIMITS.maxAbsExponent);
}

export interface JSONSize {
  nodes: number;
  depth: number;
  keys: number;
  equality: number;
  weight: number;
}

interface ByteMeter {
  add(count: number): void;
  encoded(value: unknown): void;
}

/** A value's byte budget belongs to its transport, so values are not metered. */
const unmetered: ByteMeter = { add() {}, encoded() {} };

class DescriptorBytes implements ByteMeter {
  #bytes = 0;

  add(count: number): void {
    this.#bytes += count;
    if (this.#bytes > PORTABLE_LIMITS.maxDescriptorBytes)
      exceeded("descriptor bytes", PORTABLE_LIMITS.maxDescriptorBytes);
  }

  encoded(value: unknown): void {
    if (
      typeof value === "string" &&
      value.length > PORTABLE_LIMITS.maxDescriptorBytes
    )
      exceeded("descriptor bytes", PORTABLE_LIMITS.maxDescriptorBytes);
    this.add(Buffer.byteLength(stringifyJSON(value)));
  }
}

const NULL_BYTES = 4;
const TRUE_BYTES = 4;
const FALSE_BYTES = 5;
const BRACKET_BYTES = 2;
const SEPARATOR_BYTES = 1;

// Meter while capturing so getters cannot replace an admitted value before
// cloning, compilation or validation. Payload bytes belong to the transport.
class JSONCapture {
  readonly size: JSONSize = {
    nodes: 0,
    depth: 0,
    keys: 0,
    equality: 0,
    weight: 0,
  };
  readonly #ancestors = new Set<object>();
  readonly #maxNodes: number;
  readonly #bytes: ByteMeter;

  constructor(maxNodes: number, bytes: ByteMeter) {
    this.#maxNodes = maxNodes;
    this.#bytes = bytes;
  }

  visit(value: unknown, depth: number): unknown {
    if (value instanceof Date) value = parseJSON(stringifyJSON(value));
    if (++this.size.nodes > this.#maxNodes)
      exceeded("JSON value nodes", this.#maxNodes);
    if (depth > PORTABLE_LIMITS.maxJSONDepth)
      exceeded("JSON depth", PORTABLE_LIMITS.maxJSONDepth);
    this.size.depth = Math.max(this.size.depth, depth);
    this.size.weight++;
    if (isJSONNumber(value)) {
      const lexeme =
        value instanceof ExactNumber ? value.text : value.toString();
      this.#number(lexeme);
      return value instanceof ExactNumber ? new ExactNumber(lexeme) : value;
    } else if (typeof value === "string") {
      this.size.weight += Buffer.byteLength(value);
      this.#bytes.encoded(value);
    } else if (value === null || value === undefined)
      this.#bytes.add(NULL_BYTES);
    else if (typeof value === "boolean")
      this.#bytes.add(value ? TRUE_BYTES : FALSE_BYTES);
    else return this.#container(value, depth);
    return value;
  }

  #number(lexeme: string): void {
    checkNumber(lexeme);
    this.size.weight += lexeme.length;
    // Number lexemes are ASCII; encoding the caller object would reread its text.
    this.#bytes.add(lexeme.length);
  }

  #container(value: unknown, depth: number): unknown {
    if (typeof value !== "object" || value === null)
      throw new TypeError("portable: unsupported JSON value");
    if (
      !Array.isArray(value) &&
      Object.getPrototypeOf(value) !== Object.prototype &&
      Object.getPrototypeOf(value) !== null
    )
      throw new TypeError("portable: JSON object must be a plain record");
    if (this.#ancestors.has(value))
      throw new TypeError("portable: cyclic JSON value");
    this.#ancestors.add(value);
    this.#bytes.add(BRACKET_BYTES);
    const captured = Array.isArray(value)
      ? this.#array(value, depth)
      : this.#object(value as Record<string, unknown>, depth);
    this.#ancestors.delete(value);
    return captured;
  }

  #array(value: readonly unknown[], depth: number): unknown[] {
    const length = value.length;
    if (length > this.#maxNodes - this.size.nodes)
      exceeded("JSON value nodes", this.#maxNodes);
    const startWeight = this.size.weight;
    const captured: unknown[] = [];
    // Every index becomes a JSON value; holes and undefined encode as null.
    for (let index = 0; index < length; index++) {
      if (index > 0) this.#bytes.add(SEPARATOR_BYTES);
      captured.push(this.visit(value[index], depth + 1) ?? null);
    }
    if (length > 1)
      this.size.equality = Math.min(
        PORTABLE_LIMITS.maxEqualityWork + 1,
        this.size.equality + (length - 1) * (this.size.weight - startWeight),
      );
    return captured;
  }

  #object(
    value: Record<string, unknown>,
    depth: number,
  ): Record<string, unknown> {
    const captured = Object.create(null) as Record<string, unknown>;
    let count = 0;
    for (const key of Object.keys(value)) {
      const child = value[key];
      if (child === undefined) continue;
      if (count++ > 0) this.#bytes.add(SEPARATOR_BYTES);
      this.#bytes.encoded(key);
      this.#bytes.add(SEPARATOR_BYTES);
      this.size.keys++;
      this.size.weight += Math.max(1, Buffer.byteLength(key));
      captured[key] = this.visit(child, depth + 1);
    }
    return captured;
  }
}

export function captureDescriptor(value: unknown): unknown {
  return new JSONCapture(
    PORTABLE_LIMITS.maxDescriptorNodes,
    new DescriptorBytes(),
  ).visit(value, 1);
}

export function captureValue(source: unknown): {
  value: unknown;
  size: JSONSize;
} {
  const capture = new JSONCapture(PORTABLE_LIMITS.maxValueNodes, unmetered);
  return { value: capture.visit(source, 1), size: capture.size };
}
