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

// Count before clone/stringification can recurse or numeric validation expands
// a lexeme. Payload bytes remain the transport/call owner's explicit budget.
function checkJSON(value: unknown, descriptor: boolean): JSONSize {
  const maximumNodes = descriptor
    ? PORTABLE_LIMITS.maxDescriptorNodes
    : PORTABLE_LIMITS.maxValueNodes;
  const size: JSONSize = {
    nodes: 0,
    depth: 0,
    keys: 0,
    equality: 0,
    weight: 0,
  };
  let bytes = 0;
  const ancestors = new Set<object>();
  const addBytes = (count: number) => {
    bytes += count;
    if (descriptor && bytes > PORTABLE_LIMITS.maxDescriptorBytes)
      exceeded("descriptor bytes", PORTABLE_LIMITS.maxDescriptorBytes);
  };
  const text = (value: string) => {
    if (!descriptor) return;
    if (value.length > PORTABLE_LIMITS.maxDescriptorBytes)
      exceeded("descriptor bytes", PORTABLE_LIMITS.maxDescriptorBytes);
    addBytes(Buffer.byteLength(stringifyJSON(value)));
  };
  function visit(value: unknown, depth: number): void {
    if (++size.nodes > maximumNodes) exceeded("JSON value nodes", maximumNodes);
    if (depth > PORTABLE_LIMITS.maxJSONDepth)
      exceeded("JSON depth", PORTABLE_LIMITS.maxJSONDepth);
    size.depth = Math.max(size.depth, depth);
    size.weight++;
    if (isJSONNumber(value)) {
      const number = value.toString();
      checkNumber(number);
      size.weight += number.length;
      if (descriptor) addBytes(Buffer.byteLength(stringifyJSON(value)));
      return;
    }
    if (typeof value === "string") {
      size.weight += Buffer.byteLength(value);
      text(value);
      return;
    }
    if (value === null || value === undefined || typeof value === "boolean") {
      addBytes(value === true ? 4 : value === false ? 5 : 4);
      return;
    }
    if (typeof value !== "object")
      throw new TypeError("portable: unsupported JSON value");
    if (value instanceof Date) {
      size.weight += Buffer.byteLength(stringifyJSON(value));
      if (descriptor) addBytes(Buffer.byteLength(stringifyJSON(value)));
      return;
    }
    if (
      !Array.isArray(value) &&
      Object.getPrototypeOf(value) !== Object.prototype &&
      Object.getPrototypeOf(value) !== null
    )
      throw new TypeError("portable: JSON object must be a plain record");
    if (ancestors.has(value))
      throw new TypeError("portable: cyclic JSON value");
    ancestors.add(value);
    addBytes(2);
    const startWeight = size.weight;
    let count = 0;
    if (Array.isArray(value)) {
      for (const child of value) {
        if (count++ > 0) addBytes(1);
        visit(child, depth + 1);
      }
    } else {
      for (const [key, child] of Object.entries(value)) {
        if (child === undefined) continue;
        if (count++ > 0) addBytes(1);
        text(key);
        addBytes(1);
        size.keys++;
        size.weight += Math.max(1, Buffer.byteLength(key));
        visit(child, depth + 1);
      }
    }
    if (Array.isArray(value) && count > 1)
      size.equality = Math.min(
        PORTABLE_LIMITS.maxEqualityWork + 1,
        size.equality + (count - 1) * (size.weight - startWeight),
      );
    ancestors.delete(value);
  }
  visit(value, 1);
  return size;
}

export function checkDescriptor(value: unknown): void {
  checkJSON(value, true);
}

export function checkValue(value: unknown): JSONSize {
  return checkJSON(value, false);
}
