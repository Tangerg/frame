import { describe, expect, it } from "vitest";
import { Method, digestJSON } from "./contract.js";
import { compileSchema, SchemaMismatch } from "./schema.js";

describe("admitted schema reference graph", () => {
  it("preserves reference siblings and composition in every method value", () => {
    const schema = {
      $defs: {
        "a~1b": { type: "string" },
        "a/b": { $ref: "#" },
      },
      $ref: "#/$defs/a~01b",
      minLength: 3,
      allOf: [{ pattern: "^a" }],
    };
    const descriptor = {
      name: "read",
      kind: "query" as const,
      description: "Read a matching value.",
      input: schema,
      output: schema,
      stream: schema,
    };
    const original = digestJSON(descriptor);
    const method = new Method(descriptor);
    for (const validate of [
      method.admitInput,
      method.admitOutput,
      method.admitChunk!,
    ]) {
      expect(() => validate("abc")).not.toThrow();
      for (const value of ["ab", "xbc", 7, null])
        expect(() => validate(value)).toThrow(SchemaMismatch);
    }
    expect(digestJSON(descriptor)).toBe(original);
    expect(digestJSON(method.descriptor)).toBe(original);
    expect(method.identity).toBe(
      digestJSON({ ...descriptor, description: "" }),
    );
  });

  it.each(["$defs", "definitions"])(
    "keeps author %s names separate from generated node indexes",
    (keyword) => {
      for (const name of [
        "0",
        "id",
        "$id",
        "$defs",
        "definitions",
        "__proto__",
      ]) {
        const schema = {
          [keyword]: { [name]: { const: "admitted" } },
          $ref: "#/" + keyword + "/" + name,
        };
        const validate = compileSchema(schema);
        expect(() => validate("admitted")).not.toThrow();
        expect(() => validate("different")).toThrow(SchemaMismatch);
      }
    },
  );

  it.each(["id", "$id"])(
    "refuses schema %s before engine projection",
    (keyword) => {
      expect(() =>
        compileSchema({
          [keyword]: "https://frame.invalid/schema",
          type: "string",
        }),
      ).toThrow(/unsupported schema keyword/);
    },
  );

  it("keeps references to boolean schemas exact", () => {
    for (const allowed of [true, false]) {
      const validate = compileSchema({
        $defs: { allowed },
        $ref: "#/$defs/allowed",
      });
      if (allowed) expect(() => validate({})).not.toThrow();
      else expect(() => validate({})).toThrow(SchemaMismatch);
    }
  });

  it("preserves recursion that consumes instance depth through escaped names", () => {
    const schema = {
      definitions: {
        "node~1/": {
          type: "object",
          properties: {
            count: { type: "integer" },
            next: { $ref: "#/definitions/node~01~1" },
          },
          required: ["count"],
          additionalProperties: false,
        },
      },
      $ref: "#/definitions/node~01~1",
    };
    const validate = compileSchema(schema);
    expect(() => validate({ count: 1, next: { count: 2 } })).not.toThrow();
    expect(() => validate({ count: 1, next: { count: "2" } })).toThrow(
      SchemaMismatch,
    );
    expect(() => validate({ count: 1, extra: true })).toThrow(SchemaMismatch);
  });
});
