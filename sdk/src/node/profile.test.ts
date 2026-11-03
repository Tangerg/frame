import { readFileSync } from "node:fs";
import { describe, expect, it, vi } from "vitest";
import { Validator } from "jsonschema";
import { parseJSON } from "../json.js";
import { digestJSON, freezeJSON, Method } from "./contract.js";
import { compileSchema, SchemaMismatch } from "./schema.js";

interface ProfileCase {
  name: string;
  schema: unknown;
  admitted: boolean;
  digest: string;
  values: { value: unknown; valid: boolean }[];
}

const cases = parseJSON(
  readFileSync(
    new URL("../../../portable/testdata/schema-profile.json", import.meta.url),
    "utf8",
  ),
) as ProfileCase[];

describe("shared Go and Node schema profile", () => {
  for (const sample of cases) {
    it(sample.name, () => {
      expect(digestJSON(sample.schema)).toBe(sample.digest);
      if (!sample.admitted) {
        expect(() => compileSchema(sample.schema)).toThrow();
        return;
      }
      const validate = compileSchema(sample.schema);
      expect(digestJSON(sample.schema)).toBe(sample.digest);
      for (const { value, valid } of sample.values) {
        if (valid) expect(() => validate(value)).not.toThrow();
        else expect(() => validate(value)).toThrow();
      }
    });
  }

  it("keeps the descriptor and caller members immutable during validation", () => {
    const schema = freezeJSON(
      parseJSON(`{
      "type": "object",
      "properties": {"__proto__": {"type": "string"}, "toString": {"type": "string"}},
      "additionalProperties": false
    }`),
    );
    const validate = compileSchema(schema);
    const instance = Object.freeze({});
    expect(() => validate(instance)).not.toThrow();
    expect(Object.keys(instance)).toEqual([]);
    const declared = freezeJSON(
      parseJSON('{"__proto__":"value","toString":"value"}'),
    );
    expect(() => validate(declared)).not.toThrow();
    expect(Object.getPrototypeOf(declared)).toBe(Object.prototype);
  });

  it("enforces own-member constraints at the method admission boundary", () => {
    const method = new Method({
      name: "read",
      kind: "query",
      description: "Read a bounded object.",
      input: {
        type: "object",
        properties: { ok: { type: "string" } },
        additionalProperties: false,
      },
      output: true,
    });
    for (const name of [
      "__proto__",
      "constructor",
      "toString",
      "hasOwnProperty",
    ]) {
      expect(() => method.validateInput(parseJSON(`{"${name}":7}`))).toThrow();
    }
  });

  it("applies not to method inputs, outputs, and streamed values", () => {
    const excludesNull = { not: { type: "null" } };
    const method = new Method({
      name: "read",
      kind: "query",
      description: "Read non-null values.",
      input: excludesNull,
      output: excludesNull,
      stream: excludesNull,
    });
    for (const validate of [
      method.validateInput,
      method.validateOutput,
      method.validateChunk!,
    ]) {
      for (const value of [{}, 1, "ok"])
        expect(() => validate(value)).not.toThrow();
      expect(() => validate(null)).toThrow(SchemaMismatch);
    }
    expect(
      () =>
        new Method({
          name: "read",
          kind: "query",
          description: "Read nullable values.",
          input: { not: { type: "string" } },
          output: true,
        }),
    ).toThrow("method input must exclude null");
  });

  it("does not treat an engine failure as evidence that input excludes null", () => {
    const failure = new Error("schema engine failed");
    const validate = Validator.prototype.validate;
    const engine = vi
      .spyOn(Validator.prototype, "validate")
      .mockImplementation(function (this: Validator, value, ...arguments_) {
        if (value === null) throw failure;
        return validate.call(this, value, ...arguments_);
      });
    try {
      expect(
        () =>
          new Method({
            name: "read",
            kind: "query",
            description: "Read an object.",
            input: { type: "object" },
            output: true,
          }),
      ).toThrow(failure);
    } finally {
      engine.mockRestore();
    }
  });
});
