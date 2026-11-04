import { describe, expect, it } from "vitest";
import { stringifyJSON } from "../json.js";
import { captureDescriptor, captureValue, PORTABLE_LIMITS } from "./limits.js";
import { compileSchema, SchemaMismatch } from "./schema.js";

describe("portable sparse arrays", () => {
  it("charges every encoded null before cloning a sparse descriptor", () => {
    const sparse = Array(PORTABLE_LIMITS.maxDescriptorNodes);
    expect(() => captureDescriptor(sparse)).toThrow(/profile limit/);
    expect(() => compileSchema({ type: "string", default: sparse })).toThrow(
      /profile limit/,
    );
    expect(() => captureValue(Array(PORTABLE_LIMITS.maxValueNodes))).toThrow(
      /profile limit/,
    );
  });

  it("counts the same nodes, depth and equality cost as its encoded document", () => {
    const sparse = Array(3);
    expect(stringifyJSON(sparse)).toBe("[null,null,null]");
    expect(captureValue(sparse)).toEqual(captureValue([null, null, null]));
  });

  it("validates holes as their encoded null values", () => {
    const onlyStrings = compileSchema({
      type: "array",
      items: { type: "string" },
    });
    const onlyNulls = compileSchema({ type: "array", items: { type: "null" } });
    const unique = compileSchema({ type: "array", uniqueItems: true });
    expect(() => onlyStrings(Array(1))).toThrow(SchemaMismatch);
    expect(() => onlyNulls(Array(1))).not.toThrow();
    expect(() => unique(Array(2))).toThrow(SchemaMismatch);
  });
});
