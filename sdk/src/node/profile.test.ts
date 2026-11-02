import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { parseJSON } from "../json.js";
import { digestJSON } from "./contract.js";
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
});
