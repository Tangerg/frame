import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { Decimal } from "decimal.js";
import { ExactNumber } from "../json.js";
import {
  Capability,
  Method,
  PROTOCOL,
  type MethodDescriptor,
} from "./contract.ts";
import { PORTABLE_LIMITS } from "./limits.ts";
import { compileSchema } from "./schema.ts";

const method = (name = "limits/read"): MethodDescriptor => ({
  name,
  kind: "query",
  target: "workspace",
  description: "",
  input: { type: "object" },
  output: { type: "number" },
  stream: { type: "number" },
});

function referenceChain(depth: number, duplicate = false): unknown {
  const definitions: Record<string, unknown> = { d0: true };
  for (let index = 1; index < depth; index++) {
    const ref = { $ref: "#/$defs/d" + (index - 1) };
    definitions["d" + index] = duplicate ? { allOf: [ref, ref] } : ref;
  }
  return { $defs: definitions, $ref: "#/$defs/d" + (depth - 1) };
}

function nestedObject(depth: number): unknown {
  let value: unknown = {};
  for (let index = 1; index < depth; index++) value = { next: value };
  return value;
}

describe("Go-owned portable profile budgets", () => {
  it("checks exact descriptor nodes and total bytes before cloning or compiling", () => {
    const source = {
      type: "string",
      default: Array(PORTABLE_LIMITS.maxDescriptorNodes - 3).fill(0),
    };
    expect(() => compileSchema(source)).not.toThrow();
    source.default.push(0);
    expect(() => compileSchema(source)).toThrow(/profile limit/);
    const bytes = {
      description: "x".repeat(
        PORTABLE_LIMITS.maxDescriptorBytes - '{"description":""}'.length,
      ),
    };
    expect(() => compileSchema(bytes)).not.toThrow();
    bytes.description += "x";
    expect(() => compileSchema(bytes)).toThrow(/profile limit/);
    expect(
      () =>
        new Method({
          ...method(),
          description: "x".repeat(PORTABLE_LIMITS.maxDescriptorBytes),
        }),
    ).toThrow(/profile limit/);
    const cycle: Record<string, unknown> = {};
    cycle.default = cycle;
    expect(() => compileSchema(cycle)).toThrow(/cyclic/);
  });

  it("admits the exact method capacity and refuses a larger declaration", () => {
    const descriptor = {
      protocol: PROTOCOL,
      reference: "limits",
      methods: Array.from({ length: PORTABLE_LIMITS.maxMethods }, (_, index) =>
        method("limit/read/" + index),
      ),
    };
    expect(() => new Capability(descriptor)).not.toThrow();
    descriptor.methods.push(method("over/capacity"));
    expect(() => new Capability(descriptor)).toThrow(/capability methods/);
    expect(Object.isFrozen(PORTABLE_LIMITS)).toBe(true);
  });

  it("rejects non-consuming cycles and bounds local reference expansion", () => {
    for (const source of [
      { $ref: "#" },
      { anyOf: [true, { $ref: "#" }] },
      {
        $defs: {
          a: { $ref: "#/$defs/b" },
          b: { allOf: [{ $ref: "#/$defs/a" }] },
        },
        $ref: "#/$defs/a",
      },
      { $ref: "https://example.invalid/schema" },
      { $id: "https://example.invalid/local", type: "string" },
    ])
      expect(() => compileSchema(source)).toThrow();
    expect(() =>
      compileSchema(referenceChain(PORTABLE_LIMITS.maxReferenceDepth - 1)),
    ).not.toThrow();
    expect(() =>
      compileSchema(referenceChain(PORTABLE_LIMITS.maxReferenceDepth)),
    ).toThrow(/profile limit/);
    expect(() => compileSchema(referenceChain(14, true))).toThrow(
      /profile limit/,
    );
    expect(() =>
      compileSchema({
        $defs: { value: true },
        allOf: Array.from(
          { length: PORTABLE_LIMITS.maxReferences + 1 },
          () => ({ $ref: "#/$defs/value" }),
        ),
      }),
    ).toThrow(/schema references/);
  });

  it("bounds structural recursion for query input, output, and stream identically", () => {
    const schema = {
      type: "object",
      properties: { next: { $ref: "#" } },
      additionalProperties: false,
    };
    const contract = new Method({
      ...method(),
      input: schema,
      output: schema,
      stream: schema,
    });
    for (const validate of [
      contract.validateInput,
      contract.validateOutput,
      contract.validateChunk!,
    ]) {
      expect(() => validate(nestedObject(3))).not.toThrow();
      expect(() =>
        validate(nestedObject(PORTABLE_LIMITS.maxJSONDepth)),
      ).not.toThrow();
      expect(() =>
        validate(nestedObject(PORTABLE_LIMITS.maxJSONDepth + 1)),
      ).toThrow(/JSON depth/);
    }
    const branching = compileSchema({
      type: "object",
      allOf: [
        { properties: { next: { $ref: "#" } } },
        { properties: { next: { $ref: "#" } } },
      ],
    });
    expect(() => branching(nestedObject(4))).not.toThrow();
    expect(() => branching(nestedObject(40))).toThrow(/validation work/);
  });

  it("preserves wide exact numbers but rejects hostile literals before Decimal", () => {
    const contract = new Method({
      ...method(),
      input: { type: "object", properties: { revision: { type: "number" } } },
    });
    for (const number of [
      -9223372036854775808n,
      18446744073709551615n,
      new ExactNumber("1.2300e-10"),
      new ExactNumber("1e" + PORTABLE_LIMITS.maxAbsExponent),
      new ExactNumber("9".repeat(PORTABLE_LIMITS.maxNumberBytes)),
    ]) {
      expect(() => contract.validateOutput(number)).not.toThrow();
      expect(() => contract.validateChunk!(number)).not.toThrow();
    }
    for (const raw of [
      "1e1000000000",
      "1e-1000000000",
      "1e" + (PORTABLE_LIMITS.maxAbsExponent + 1),
      "9".repeat(PORTABLE_LIMITS.maxNumberBytes + 1),
    ]) {
      const number = new ExactNumber(raw);
      expect(() => contract.validateInput({ revision: number })).toThrow(
        /profile limit/,
      );
      expect(() => contract.validateOutput(number)).toThrow(/profile limit/);
      expect(() => contract.validateChunk!(number)).toThrow(/profile limit/);
      for (const source of [
        { minimum: number },
        { maximum: number },
        { const: number },
        { enum: [number] },
      ])
        expect(() => compileSchema(source)).toThrow(/profile limit/);
    }
  });

  it("keeps large chat bodies and vector batches under their caller byte budgets", () => {
    const vectors = compileSchema({
      type: "object",
      properties: {
        space: { type: "string" },
        vectors: {
          type: "array",
          items: { type: "array", items: { type: "number" } },
        },
      },
      required: ["space", "vectors"],
    });
    expect(() =>
      vectors({
        space: "captured-model",
        vectors: Array.from({ length: 64 }, () => Array<number>(3072).fill(0)),
      }),
    ).not.toThrow();
    const chat = compileSchema({
      type: "object",
      properties: {
        messages: {
          type: "array",
          items: {
            type: "object",
            properties: {
              role: { enum: ["user", "assistant"] },
              content: { type: "string" },
            },
          },
        },
      },
      required: ["messages"],
    });
    const input = {
      messages: Array.from({ length: 128 }, () => ({
        role: "user",
        content: "x".repeat(16 * 1024),
      })),
    };
    expect(JSON.stringify(input).length).toBeGreaterThan(
      PORTABLE_LIMITS.maxDescriptorBytes,
    );
    expect(() => chat(input)).not.toThrow();
  });

  it("keeps schema arithmetic exact at admitted bounds and independent of Decimal globals", () => {
    const previous = {
      precision: Decimal.precision,
      minE: Decimal.minE,
      maxE: Decimal.maxE,
      rounding: Decimal.rounding,
      modulo: Decimal.modulo,
    };
    try {
      Decimal.set({ precision: 1, minE: -1, maxE: 1 });
      const exponent = PORTABLE_LIMITS.maxAbsExponent;
      const unit = new ExactNumber(`1e-${exponent}`);
      const validate = compileSchema({
        type: "number",
        minimum: unit,
        maximum: new ExactNumber(`1e${exponent}`),
        multipleOf: unit,
      });
      for (const raw of [`1e-${exponent}`, `2e-${exponent}`, `1e${exponent}`])
        expect(() => validate(new ExactNumber(raw))).not.toThrow();
      for (const raw of [
        `1.1e-${exponent}`,
        `0.1e-${exponent}`,
        `2e${exponent}`,
      ])
        expect(() => validate(new ExactNumber(raw))).toThrow(
          /does not satisfy/,
        );
      const indivisible = compileSchema({
        multipleOf: new ExactNumber("9".repeat(PORTABLE_LIMITS.maxNumberBytes)),
      });
      expect(() => indivisible(1)).toThrow(/does not satisfy/);
    } finally {
      Decimal.set(previous);
    }
  });

  it("keeps fixed identifier patterns without admitting variable backtracking", () => {
    for (const pattern of [
      String.raw`\S`,
      "^[0-9a-fA-F]{40}$",
      "^mem_[0-9a-f]{32}$",
    ])
      expect(() => compileSchema({ type: "string", pattern })).not.toThrow();
    for (const pattern of [
      "(a+)+$",
      "a|aa",
      "a*",
      "a{1,100}",
      String.raw`(a)\1`,
      "a{1000000000}",
    ])
      expect(() => compileSchema({ type: "string", pattern })).toThrow();
  });

  it("accounts for equality text costs only for relevant schemas", () => {
    const large = "x".repeat(PORTABLE_LIMITS.maxEqualityWork / 8);
    const values = ["a" + large, "b" + large, "c" + large, "d" + large];
    const source = { enum: values };
    expect(JSON.stringify(source).length).toBeLessThan(
      PORTABLE_LIMITS.maxDescriptorBytes,
    );
    expect(() => compileSchema(source)).toThrow(/equality work/);
    const unique = compileSchema({ type: "array", uniqueItems: true });
    const ordinary = compileSchema({ type: "array" });
    const keys = values.map((_, index) => ({ [index + large]: index }));
    const numbers = Array.from(
      { length: 64 },
      () => new ExactNumber("9".repeat(512)),
    );
    for (const value of [values, keys, numbers]) {
      expect(() => ordinary(value)).not.toThrow();
      expect(() => unique(value)).toThrow(/equality work/);
    }
    expect(() => unique([1, 2, 3])).not.toThrow();
    expect(() => unique([1, 1])).toThrow(/does not satisfy/);
  });
});

it("checks the portable size corpus without changing descriptor lexemes", () => {
  const cases = JSON.parse(
    readFileSync(
      new URL("../../../portable/testdata/schema_sizes.json", import.meta.url),
      "utf8",
    ),
  ) as { literal: string; admitted: boolean }[];
  for (const sample of cases) {
    const bound = new ExactNumber(sample.literal);
    for (const keyword of [
      "minLength",
      "maxLength",
      "minItems",
      "maxItems",
      "minProperties",
      "maxProperties",
    ]) {
      const schema = { [keyword]: bound };
      if (sample.admitted) expect(() => compileSchema(schema)).not.toThrow();
      else expect(() => compileSchema(schema)).toThrow();
      expect(schema[keyword]!.text).toBe(sample.literal);
    }
  }
  const validate = compileSchema({ minLength: new ExactNumber("2.0") });
  validate("ab");
  expect(() => validate("😀")).toThrow();
});
