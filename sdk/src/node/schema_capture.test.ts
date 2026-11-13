import { describe, expect, it } from "vitest";
import { ExactNumber, parseJSON, stringifyJSON } from "../json.js";
import { Capability, digestJSON, Method, PROTOCOL } from "./contract.js";
import { captureDescriptor, PORTABLE_LIMITS } from "./limits.js";
import { compileSchema, SchemaMismatch } from "./schema.js";

describe("portable JSON admission capture", () => {
  it("retains repeated enum members and their lexemes through capability admission", () => {
    const schema = parseJSON('{"enum":[1,1.0,1e0]}');
    const descriptor = {
      protocol: PROTOCOL,
      reference: "numbers.repeated-enum",
      methods: [
        {
          name: "read",
          kind: "query",
          description: "Read an original numeric enum member.",
          input: schema,
          output: schema,
          stream: schema,
        },
      ],
    } as const;
    const original = stringifyJSON(descriptor);
    const capability = new Capability(descriptor);
    const method = capability.method("read");

    expect(stringifyJSON(capability.descriptor)).toBe(original);
    expect(stringifyJSON(descriptor)).toBe(original);
    expect(method.identity).toBe(
      digestJSON({ ...descriptor.methods[0], description: "" }),
    );
    for (const admit of [
      method.admitInput,
      method.admitOutput,
      method.admitChunk!,
    ]) {
      expect(stringifyJSON(admit(new ExactNumber("1.00")))).toBe("1.00");
      expect(() => admit(2)).toThrow(SchemaMismatch);
      expect(() => admit("1")).toThrow(SchemaMismatch);
    }
  });

  it("meters the retained numeric lexeme once at the descriptor byte boundary", () => {
    const lexeme = "9999999999999999";
    const overhead = Buffer.byteLength('{"const":0,"description":""}');
    const descriptor = (padding: number) => {
      let reads = 0;
      const numeric = Object.create(ExactNumber.prototype, {
        text: { get: () => (++reads === 1 ? lexeme : "0") },
      });
      return {
        source: { const: numeric, description: "x".repeat(padding) },
        reads: () => reads,
      };
    };
    const oversized = descriptor(PORTABLE_LIMITS.maxDescriptorBytes - overhead);
    expect(() => captureDescriptor(oversized.source)).toThrow(
      /descriptor bytes/,
    );
    expect(oversized.reads()).toBe(1);

    const boundary = descriptor(
      PORTABLE_LIMITS.maxDescriptorBytes - overhead - (lexeme.length - 1),
    );
    const captured = captureDescriptor(boundary.source);
    expect(boundary.reads()).toBe(1);
    expect(captured).toMatchObject({ const: { text: lexeme } });
    expect(Buffer.byteLength(stringifyJSON(captured))).toBe(
      PORTABLE_LIMITS.maxDescriptorBytes,
    );
  });

  it("meters and validates the same array element", () => {
    let reads = 0;
    const input = ["initial"];
    Object.defineProperty(input, "0", {
      enumerable: true,
      get: () => (++reads === 1 ? "accepted" : "changed"),
    });
    const validate = compileSchema({
      type: "array",
      items: { const: "accepted" },
    });
    expect(() => validate(input)).not.toThrow();
    expect(reads).toBe(1);
  });

  it("compiles the descriptor captured by its bounded admission", () => {
    let reads = 0;
    const validate = compileSchema({
      get type() {
        return ++reads === 1 ? "string" : "number";
      },
    });
    expect(reads).toBe(1);
    expect(() => validate("accepted")).not.toThrow();
    expect(() => validate(1)).toThrow(SchemaMismatch);
  });

  it("derives a method's descriptor, validator and identity from one capture", () => {
    let reads = 0;
    const stable = {
      name: "inspect",
      kind: "query" as const,
      description: "",
      input: { type: "string" },
      output: { type: "string" },
    };
    const method = new Method({
      ...stable,
      get input() {
        return ++reads < 3 ? { type: "string" } : { type: "number" };
      },
    });
    expect(reads).toBe(1);
    expect(method.descriptor).toEqual(stable);
    expect(method.identity).toBe(new Method(stable).identity);
    expect(() => method.admitInput("accepted")).not.toThrow();
    expect(() => method.admitInput(1)).toThrow(SchemaMismatch);
  });

  it("checks and retains the same capability reference", () => {
    let reads = 0;
    const capability = new Capability({
      protocol: PROTOCOL,
      get reference() {
        return ++reads === 1 ? "accepted" : "changed";
      },
      methods: [
        {
          name: "inspect",
          kind: "query",
          description: "",
          input: { type: "string" },
          output: { type: "string" },
        },
      ],
    });
    expect(reads).toBe(1);
    expect(capability.descriptor.reference).toBe("accepted");
  });
});
