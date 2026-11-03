import { isJSONNumber, stringifyJSON } from "../json.js";
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

/** Accounts for the encoded size of a document while it is counted. */
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

// The encoded length of each JSON literal and separator.
const NULL_BYTES = 4;
const TRUE_BYTES = 4;
const FALSE_BYTES = 5;
const BRACKET_BYTES = 2;
const SEPARATOR_BYTES = 1;

// Count before clone/stringification can recurse or numeric validation expands
// a lexeme. Payload bytes remain the transport/call owner's explicit budget.
class JSONCounter {
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

  visit(value: unknown, depth: number): void {
    if (++this.size.nodes > this.#maxNodes)
      exceeded("JSON value nodes", this.#maxNodes);
    if (depth > PORTABLE_LIMITS.maxJSONDepth)
      exceeded("JSON depth", PORTABLE_LIMITS.maxJSONDepth);
    this.size.depth = Math.max(this.size.depth, depth);
    this.size.weight++;
    if (isJSONNumber(value)) this.#number(value.toString(), value);
    else if (typeof value === "string") {
      this.size.weight += Buffer.byteLength(value);
      this.#bytes.encoded(value);
    } else if (value === null || value === undefined)
      this.#bytes.add(NULL_BYTES);
    else if (typeof value === "boolean")
      this.#bytes.add(value ? TRUE_BYTES : FALSE_BYTES);
    else if (value instanceof Date) {
      this.size.weight += Buffer.byteLength(stringifyJSON(value));
      this.#bytes.encoded(value);
    } else this.#container(value, depth);
  }

  #number(lexeme: string, value: unknown): void {
    checkNumber(lexeme);
    this.size.weight += lexeme.length;
    this.#bytes.encoded(value);
  }

  #container(value: unknown, depth: number): void {
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
    if (Array.isArray(value)) this.#array(value, depth);
    else this.#object(value as Record<string, unknown>, depth);
    this.#ancestors.delete(value);
  }

  /** Charges an array for comparing its elements for uniqueness. */
  #array(value: readonly unknown[], depth: number): void {
    const startWeight = this.size.weight;
    value.forEach((child, index) => {
      if (index > 0) this.#bytes.add(SEPARATOR_BYTES);
      this.visit(child, depth + 1);
    });
    if (value.length > 1)
      this.size.equality = Math.min(
        PORTABLE_LIMITS.maxEqualityWork + 1,
        this.size.equality +
          (value.length - 1) * (this.size.weight - startWeight),
      );
  }

  #object(value: Record<string, unknown>, depth: number): void {
    let count = 0;
    for (const [key, child] of Object.entries(value)) {
      if (child === undefined) continue;
      if (count++ > 0) this.#bytes.add(SEPARATOR_BYTES);
      this.#bytes.encoded(key);
      this.#bytes.add(SEPARATOR_BYTES);
      this.size.keys++;
      this.size.weight += Math.max(1, Buffer.byteLength(key));
      this.visit(child, depth + 1);
    }
  }
}

export function checkDescriptor(value: unknown): void {
  new JSONCounter(
    PORTABLE_LIMITS.maxDescriptorNodes,
    new DescriptorBytes(),
  ).visit(value, 1);
}

export function checkValue(value: unknown): JSONSize {
  const counter = new JSONCounter(PORTABLE_LIMITS.maxValueNodes, unmetered);
  counter.visit(value, 1);
  return counter.size;
}
