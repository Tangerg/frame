import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { parseJSON } from "../json.js";
import { digestJSON, freezeJSON, Method } from "./contract.js";
import { compileSchema } from "./schema.js";

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
});
