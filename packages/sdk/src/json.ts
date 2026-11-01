import {
  applyEdits,
  format,
  parseTree,
  printParseErrorCode,
  visit,
  type Node,
} from "jsonc-parser";

// Unicode mode consumes a valid surrogate pair as one code point.
const unpairedSurrogate = /\p{Surrogate}/u;

function validateJSON(text: string): void {
  const objects: Array<Set<string>> = [];
  visit(
    text,
    {
      onObjectBegin() {
        objects.push(new Set());
      },
      onObjectProperty(name, offset) {
        if (unpairedSurrogate.test(name)) {
          throw new SyntaxError(
            "invalid Unicode in JSON member at offset " + offset,
          );
        }
        const members = objects.at(-1)!;
        if (members.has(name)) {
          throw new SyntaxError("duplicate JSON member at offset " + offset);
        }
        members.add(name);
      },
      onObjectEnd() {
        objects.pop();
      },
      onLiteralValue(value: unknown, offset) {
        if (typeof value === "string" && unpairedSurrogate.test(value)) {
          throw new SyntaxError(
            "invalid Unicode in JSON string at offset " + offset,
          );
        }
      },
      onError(code, offset) {
        throw new SyntaxError(
          printParseErrorCode(code) + " at offset " + offset,
        );
      },
    },
    {
      disallowComments: true,
      allowTrailingComma: false,
      allowEmptyContent: false,
    },
  );
}

// A feature owns arbitrary numeric ranges and receipt lexemes. Values that a
// JS number cannot round-trip stay textual without expanding huge exponents.
export class ExactNumber {
  readonly text: string;

  constructor(text: string) {
    validateJSON(text);
    const node = parseTree(text)!;
    if (
      node.type !== "number" ||
      node.offset !== 0 ||
      node.length !== text.length
    )
      throw new TypeError("exact number requires one JSON number literal");
    this.text = text;
    Object.freeze(this);
  }

  toString(): string {
    return this.text;
  }

  toJSON(): never {
    throw new TypeError("exact JSON numbers require stringifyJSON");
  }
}

export type JSONNumber = number | bigint | ExactNumber;

export function isJSONNumber(value: unknown): value is JSONNumber {
  return (
    (typeof value === "number" && Number.isFinite(value)) ||
    typeof value === "bigint" ||
    value instanceof ExactNumber
  );
}

// Exponents stay textual evidence; comparison must neither expand them nor
// underflow a nonzero value into another value's identity.
class DecimalValue {
  readonly negative: boolean;
  readonly digits: string;
  readonly exponent: bigint;

  constructor(value: JSONNumber) {
    if (!isJSONNumber(value))
      throw new TypeError("numeric comparison requires a JSON number");
    const literal = value.toString();
    const exponentAt = literal.search(/[eE]/);
    const mantissa = exponentAt < 0 ? literal : literal.slice(0, exponentAt);
    const pointAt = mantissa.indexOf(".");
    const fraction = pointAt < 0 ? 0 : mantissa.length - pointAt - 1;
    const coefficient = mantissa.replace("-", "").replace(".", "");
    this.digits = coefficient.replace(/^0+/, "").replace(/0+$/, "");
    this.negative = this.digits.length > 0 && literal.startsWith("-");
    const trailingZeros =
      coefficient.length - coefficient.replace(/0+$/, "").length;
    this.exponent =
      this.digits.length === 0
        ? 0n
        : (exponentAt < 0 ? 0n : BigInt(literal.slice(exponentAt + 1))) -
          BigInt(fraction) +
          BigInt(trailingZeros);
  }

  compare(other: DecimalValue): number {
    if (this.negative !== other.negative) return this.negative ? -1 : 1;
    let magnitude: number;
    if (this.digits.length === 0 || other.digits.length === 0) {
      magnitude = Math.sign(this.digits.length - other.digits.length);
    } else {
      const leftOrder = BigInt(this.digits.length) + this.exponent;
      const rightOrder = BigInt(other.digits.length) + other.exponent;
      if (leftOrder !== rightOrder) magnitude = leftOrder < rightOrder ? -1 : 1;
      else {
        const width = Math.max(this.digits.length, other.digits.length);
        const left = this.digits.padEnd(width, "0");
        const right = other.digits.padEnd(width, "0");
        magnitude = left === right ? 0 : left < right ? -1 : 1;
      }
    }
    return magnitude === 0 ? 0 : this.negative ? -magnitude : magnitude;
  }

  isInteger(): boolean {
    return this.exponent >= 0n;
  }
}

export function compareJSONNumbers(
  left: JSONNumber,
  right: JSONNumber,
): number {
  return new DecimalValue(left).compare(new DecimalValue(right));
}

export function isJSONInteger(value: unknown): boolean {
  return isJSONNumber(value) && new DecimalValue(value).isInteger();
}

export function equalJSON(left: unknown, right: unknown): boolean {
  if (isJSONNumber(left) || isJSONNumber(right))
    return (
      isJSONNumber(left) &&
      isJSONNumber(right) &&
      compareJSONNumbers(left, right) === 0
    );
  if (Array.isArray(left) || Array.isArray(right))
    return (
      Array.isArray(left) &&
      Array.isArray(right) &&
      left.length === right.length &&
      left.every((value, index) => equalJSON(value, right[index]))
    );
  if (
    left !== null &&
    right !== null &&
    typeof left === "object" &&
    typeof right === "object"
  ) {
    const a = left as Record<string, unknown>,
      b = right as Record<string, unknown>;
    return (
      Object.keys(a).length === Object.keys(b).length &&
      Object.keys(a).every(
        (key) => Object.hasOwn(b, key) && equalJSON(a[key], b[key]),
      )
    );
  }
  return left === right;
}

// A typed wire DTO promises JavaScript numbers. Exact query JSON retains the
// received evidence; this projection refuses values the DTO cannot represent.
export function projectNumber(value: unknown): number {
  if (!isJSONNumber(value))
    throw new TypeError("wire projection requires a JSON number");
  if (
    typeof value === "number" &&
    (!Number.isFinite(value) ||
      (Number.isInteger(value) && !Number.isSafeInteger(value)))
  )
    throw new RangeError(
      "wire number is not exactly representable; consume exact query JSON",
    );
  if (value instanceof ExactNumber || typeof value === "bigint") {
    const projected = Number(value.toString());
    if (
      !Number.isFinite(projected) ||
      (Number.isInteger(projected) && !Number.isSafeInteger(projected)) ||
      compareJSONNumbers(value, projected) !== 0
    )
      throw new RangeError(
        "wire number is not exactly representable; consume exact query JSON",
      );
    return projected;
  }
  return value;
}

// Structured cloning strips ExactNumber's encoding from captured JSON evidence.
export function cloneJSON<T>(value: T): T {
  return value === undefined ? value : (parseJSON(stringifyJSON(value)) as T);
}

export function parseJSON(text: string): unknown {
  validateJSON(text);
  function read(node: Node): unknown {
    if (node.type === "number") {
      const raw = text.slice(node.offset, node.offset + node.length);
      const value = node.value as number;
      if (
        Number.isFinite(value) &&
        (!Number.isInteger(value) || Number.isSafeInteger(value)) &&
        JSON.stringify(value) === raw
      )
        return value;
      if (/^-?\d+$/.test(raw) && raw !== "-0") return BigInt(raw);
      return new ExactNumber(raw);
    }
    if (node.type === "array") return (node.children ?? []).map(read);
    if (node.type === "object") {
      const value: Record<string, unknown> = {};
      for (const property of node.children ?? []) {
        Object.defineProperty(value, property.children![0]!.value as string, {
          value: read(property.children![1]!),
          enumerable: true,
          configurable: true,
          writable: true,
        });
      }
      return value;
    }
    return node.value as unknown;
  }
  return read(parseTree(text)!);
}

export function stringifyJSON(value: unknown, sortKeys = false): string {
  const ancestors = new Set<object>();
  function write(value: unknown): string | undefined {
    if (value === null) return "null";
    if (typeof value === "string" || typeof value === "boolean")
      return JSON.stringify(value);
    if (typeof value === "bigint") return value.toString();
    if (typeof value === "number") {
      if (
        !Number.isFinite(value) ||
        (Number.isInteger(value) && !Number.isSafeInteger(value))
      )
        throw new TypeError("unsafe JSON number; use bigint or ExactNumber");
      return JSON.stringify(value);
    }
    if (value === undefined) return undefined;
    if (typeof value !== "object")
      throw new TypeError("unsupported JSON value");
    if (value instanceof ExactNumber) return value.text;
    if (value instanceof Date) return JSON.stringify(value.toJSON());
    if (
      !Array.isArray(value) &&
      Object.getPrototypeOf(value) !== Object.prototype &&
      Object.getPrototypeOf(value) !== null
    )
      throw new TypeError("JSON object must be a plain record");
    if (ancestors.has(value)) throw new TypeError("cyclic JSON value");
    ancestors.add(value);
    try {
      if (Array.isArray(value))
        return `[${Array.from(value, (item) => write(item) ?? "null").join(",")}]`;
      const fields: string[] = [];
      const entries = Object.entries(value);
      if (sortKeys)
        entries.sort(([left], [right]) =>
          left < right ? -1 : left > right ? 1 : 0,
        );
      for (const [key, field] of entries) {
        const encoded = write(field);
        if (encoded !== undefined)
          fields.push(`${JSON.stringify(key)}:${encoded}`);
      }
      return `{${fields.join(",")}}`;
    } finally {
      ancestors.delete(value);
    }
  }
  const text = write(value);
  if (text === undefined) throw new TypeError("value is not JSON serializable");
  validateJSON(text);
  return text;
}

export function prettyJSON(value: unknown): string {
  return formatJSONText(stringifyJSON(value));
}

// Change whitespace alone so displayed evidence retains its numeric lexemes.
export function formatJSONText(source: string): string {
  return applyEdits(
    source,
    format(source, undefined, { tabSize: 2, insertSpaces: true, eol: "\n" }),
  );
}
